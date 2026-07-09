// Copyright 2026 Red Hat, LLC. and/or its affiliates
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/openshift-psap/composite-dra-driver/pkg/config"
	_ "github.com/openshift-psap/composite-dra-driver/pkg/metrics"
	"github.com/openshift-psap/composite-dra-driver/pkg/plugin"
	"github.com/openshift-psap/composite-dra-driver/pkg/shadow"
	"github.com/openshift-psap/composite-dra-driver/pkg/store"
	"github.com/openshift-psap/composite-dra-driver/pkg/synthesizer"
)

func main() {
	klog.InitFlags(nil)

	var (
		configPath  string
		nodeName    string
		kubeconfig  string
		pluginDir   string
		stateDir    string
		metricsPort int
	)

	flag.StringVar(&configPath, "config", "/etc/composite-dra/config.yaml", "path to driver config")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "node name (defaults to NODE_NAME env)")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (optional, uses in-cluster if empty)")
	flag.StringVar(&pluginDir, "plugin-dir", "/var/lib/kubelet/plugins", "kubelet plugins directory")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/composite-dra", "directory for persistent state")
	flag.IntVar(&metricsPort, "metrics-port", 8080, "port for Prometheus metrics endpoint")
	flag.Parse()

	if nodeName == "" {
		klog.Fatal("node name required: set --node-name or NODE_NAME env")
	}

	cfg, err := config.LoadFromFile(configPath)
	if err != nil {
		klog.Fatalf("load config: %v", err)
	}
	if err := config.Validate(cfg); err != nil {
		klog.Fatalf("invalid config: %v", err)
	}

	restConfig, err := buildRESTConfig(kubeconfig)
	if err != nil {
		klog.Fatalf("build REST config: %v", err)
	}
	restConfig.QPS = 100
	restConfig.Burst = 200

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("create kube client: %v", err)
	}

	checkKubeVersion(kubeClient.Discovery())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := os.MkdirAll(stateDir, 0755); err != nil {
		klog.Fatalf("create state dir: %v", err)
	}

	stateStore, err := store.NewStateStore(filepath.Join(stateDir, "state.db"))
	if err != nil {
		klog.Fatalf("open state store: %v", err)
	}
	defer stateStore.Close()

	deviceStore := store.NewDeviceStore()

	claimMgr := shadow.NewClaimManager(kubeClient.ResourceV1(), cfg.Driver.Name)

	var paramsResolver *shadow.DeviceParamsResolver
	if cfg.DeviceParams != nil {
		nodeLabels := synthesizer.FetchNodeLabels(kubeClient, nodeName)
		var err2 error
		paramsResolver, err2 = shadow.NewDeviceParamsResolver(
			cfg.DeviceParams.ConfigMapPath, nodeName, nodeLabels)
		if err2 != nil {
			klog.Fatalf("device params resolver: %v", err2)
		}
	}

	grpcClient := plugin.NewGRPCClient(pluginDir)
	defer grpcClient.Close()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	defer eventBroadcaster.Shutdown()
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: cfg.Driver.Name})

	compositePlugin := plugin.NewCompositePlugin(
		cfg.Driver.Name,
		deviceStore,
		claimMgr,
		paramsResolver,
		grpcClient,
		stateStore,
		eventRecorder,
	)

	pluginSocketDir := filepath.Join(pluginDir, cfg.Driver.Name)
	if err := os.MkdirAll(pluginSocketDir, 0755); err != nil {
		klog.Fatalf("create plugin socket dir %s: %v", pluginSocketDir, err)
	}

	klog.InfoS("composite-dra-driver starting", "node", nodeName, "driver", cfg.Driver.Name)

	helper, err := kubeletplugin.Start(ctx, compositePlugin,
		kubeletplugin.DriverName(cfg.Driver.Name),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.KubeClient(kubeClient),
	)
	if err != nil {
		klog.Fatalf("start kubelet plugin: %v", err)
	}

	publisher := synthesizer.NewHelperPublisher(func(resources resourceslice.DriverResources) error {
		return helper.PublishResources(ctx, resources)
	})

	synth := synthesizer.New(cfg, nodeName, kubeClient, deviceStore, publisher)

	synth.SetPreparedDevicesFunc(func() map[string][]struct{ SourceName, Device string } {
		raw := compositePlugin.PreparedDevicesByComposition()
		result := make(map[string][]struct{ SourceName, Device string }, len(raw))
		for comp, devs := range raw {
			for _, d := range devs {
				result[comp] = append(result[comp], struct{ SourceName, Device string }{d.SourceName, d.Device})
			}
		}
		return result
	})
	compositePlugin.SetOnStateChange(synth.Recompute)

	go func() {
		if err := synth.Start(ctx); err != nil {
			klog.Fatalf("synthesizer: %v", err)
		}
	}()

	go plugin.StartBindingWatcher(ctx, kubeClient, compositePlugin)

	go plugin.StartReconciler(ctx, kubeClient.ResourceV1(), cfg.Driver.Name, 5*time.Minute)

	metricsAddr := fmt.Sprintf(":%d", metricsPort)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: metricsAddr, Handler: metricsMux}
	go func() {
		klog.InfoS("metrics server listening", "addr", metricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "metrics server failed")
		}
	}()

	<-ctx.Done()
	klog.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		klog.ErrorS(err, "metrics server graceful shutdown failed")
	}
}

func checkKubeVersion(disco discovery.DiscoveryInterface) {
	info, err := disco.ServerVersion()
	if err != nil {
		klog.ErrorS(err, "could not query API server version")
		return
	}
	minor, _ := strconv.Atoi(strings.TrimRight(info.Minor, "+"))
	if minor >= 36 {
		klog.InfoS("DRAExtendedResource feature gate available, webhook not needed",
			"version", fmt.Sprintf("%s.%s", info.Major, info.Minor))
	}
}

func buildRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
