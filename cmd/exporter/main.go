// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cgroup"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/config"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/cri"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/device"
	bpf "github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/ebpf"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/kvm"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/metricstls"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/qga"
	"github.com/openshift-virtualization/kubevirt-metrics-exporter/pkg/qmp"
)

func main() {
	cfg := config.Parse()
	setupLogging(cfg.LogLevel)

	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	slog.Info("starting kubevirt-metrics-exporter",
		"node", cfg.NodeName,
		"qmp", cfg.EnableQMP,
		"qga", cfg.EnableQGA,
		"ebpf", cfg.EnableEBPF,
		"kvm", cfg.EnableKVM,
		"cgroup", cfg.EnableCgroup,
	)

	log := slog.Default()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	stores := startInformers(ctx, cfg.NodeName, log)

	if cfg.EnableQMP {
		startQMP(ctx, cfg, stores.podStore, stores.dynClient, log)
	}

	if cfg.EnableQGA {
		startQGA(ctx, cfg, stores.podStore, stores.dynClient, log)
	}

	if cfg.EnableKVM {
		startKVM(ctx, cfg, stores.podStore, log)
	}

	if cfg.EnableCgroup {
		startCgroup(ctx, cfg, stores.podStore, log)
	}

	if cfg.EnableEBPF {
		startEBPF(ctx, cfg, stores, log)
	}

	mux := metricsHandler(cfg.HealthListenAddress == "")

	var tlsConfig *tls.Config
	if cfg.TLSCertFile != "" {
		var pool *metricstls.ClientCAPool
		var err error
		if cfg.TLSClientCAFile != "" {
			pool, err = metricstls.LoadClientCAFile(cfg.TLSClientCAFile)
		} else {
			pool, err = metricstls.StartClientCAWatcher(ctx, stores.clientset)
		}
		if err != nil {
			slog.Error("configure metrics mTLS", "error", err)
			os.Exit(1)
		}
		tlsConfig, err = metricstls.ServerConfig(cfg.TLSCertFile, cfg.TLSKeyFile, pool, cfg.TLSMinVersion, cfg.TLSCipherSuites)
		if err != nil {
			slog.Error("configure metrics TLS policy", "error", err)
			os.Exit(1)
		}
	}

	handler := http.Handler(mux)
	if tlsConfig != nil {
		handler = metricstls.AllowPrometheusK8s(handler)
	}
	srv := &http.Server{Addr: cfg.ListenAddress, Handler: handler, TLSConfig: tlsConfig}

	var healthSrv *http.Server
	var healthListener net.Listener
	if cfg.HealthListenAddress != "" {
		var healthErr error
		healthListener, healthErr = net.Listen("tcp", cfg.HealthListenAddress)
		if healthErr != nil {
			slog.Error("listen on health endpoint", "address", cfg.HealthListenAddress, "error", healthErr)
			os.Exit(1)
		}
		healthSrv = &http.Server{Addr: cfg.HealthListenAddress, Handler: healthHandler()}
		go func() {
			if err := healthSrv.Serve(healthListener); err != nil && err != http.ErrServerClosed {
				slog.Error("health server error", "error", err)
			}
		}()
		slog.Info("health server starting", "address", cfg.HealthListenAddress)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if healthSrv != nil {
			healthSrv.Shutdown(shutdownCtx)
		}
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("metrics server starting", "address", cfg.ListenAddress, "tls", tlsConfig != nil)
	var serveErr error
	if tlsConfig != nil {
		serveErr = srv.ListenAndServeTLS("", "")
	} else {
		serveErr = srv.ListenAndServe()
	}
	if serveErr != http.ErrServerClosed {
		slog.Error("server error", "error", serveErr)
		os.Exit(1)
	}
}

type informerStores struct {
	podStore   cache.Store
	pvcIndexer cache.Indexer
	dynClient  dynamic.Interface
	clientset  kubernetes.Interface
}

func startInformers(ctx context.Context, nodeName string, log *slog.Logger) informerStores {
	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Error("building in-cluster config", "error", err)
		os.Exit(1)
	}

	cs, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		log.Error("creating clientset", "error", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(k8sCfg)
	if err != nil {
		log.Error("creating dynamic client", "error", err)
		os.Exit(1)
	}

	podFactory := informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("spec.nodeName=%s", nodeName)
		}),
	)
	podStore := podFactory.Core().V1().Pods().Informer().GetStore()

	pvcFactory := informers.NewSharedInformerFactory(cs, 0)
	pvcInformer := pvcFactory.Core().V1().PersistentVolumeClaims().Informer()
	pvcInformer.AddIndexers(cache.Indexers{
		device.PVCByPVIndexName: device.PVCByPVIndexFunc,
	})
	pvcIndexer := pvcInformer.GetIndexer()

	podFactory.Start(ctx.Done())
	pvcFactory.Start(ctx.Done())
	podFactory.WaitForCacheSync(ctx.Done())
	pvcFactory.WaitForCacheSync(ctx.Done())

	log.Info("informers synced")
	return informerStores{podStore: podStore, pvcIndexer: pvcIndexer, dynClient: dynClient, clientset: cs}
}

func startQMP(ctx context.Context, cfg *config.Config, podStore cache.Store, dynClient dynamic.Interface, log *slog.Logger) {
	criClient, err := cri.NewClient(cfg.CRISocket)
	if err != nil {
		log.Error("qmp: creating CRI client", "error", err)
		os.Exit(1)
	}

	collector := qmp.NewCollector(qmp.PollerConfig{
		NodeName:     cfg.NodeName,
		PollInterval: cfg.QMPPollInterval,
		BoundariesNs: cfg.BoundariesNs,
		QMPTimeout:   cfg.QMPTimeout,
		Concurrency:  cfg.QMPConcurrency,
		Namespaces:   config.ParseNamespaces(cfg.Namespaces),
		LabelFilter:  cfg.QMPLabelFilter,
	}, podStore, criClient, dynClient, log)

	prometheus.MustRegister(collector)
	go collector.Run(ctx)

	log.Info("qmp: subsystem started")
}

func startQGA(ctx context.Context, cfg *config.Config, podStore cache.Store, dynClient dynamic.Interface, log *slog.Logger) {
	criClient, err := cri.NewClient(cfg.CRISocket)
	if err != nil {
		log.Error("qga: creating CRI client", "error", err)
		os.Exit(1)
	}

	collector := qga.NewCollector(qga.CollectorConfig{
		NodeName:     cfg.NodeName,
		PollInterval: cfg.QGAPollInterval,
		QGATimeout:   cfg.QGATimeout,
		ExecWait:     cfg.QGAExecWait,
		MaxRetries:   cfg.QGARetries,
		Concurrency:  cfg.QGAConcurrency,
		Namespaces:   config.ParseNamespaces(cfg.Namespaces),
		LabelFilter:  cfg.QGALabelFilter,
	}, podStore, criClient, dynClient, log)

	prometheus.MustRegister(collector)
	go collector.Run(ctx)

	log.Info("qga: subsystem started")
}

func startKVM(ctx context.Context, cfg *config.Config, podStore cache.Store, log *slog.Logger) {
	collector := kvm.NewCollector(kvm.Config{
		NodeName:     cfg.NodeName,
		PollInterval: cfg.KVMPollInterval,
		DebugFSPath:  "/sys/kernel/debug/kvm",
	}, podStore, log)

	prometheus.MustRegister(collector)
	go collector.Run(ctx)

	log.Info("kvm: subsystem started")
}

func startCgroup(ctx context.Context, cfg *config.Config, podStore cache.Store, log *slog.Logger) {
	criClient, err := cri.NewClient(cfg.CRISocket)
	if err != nil {
		log.Error("cgroup: creating CRI client", "error", err)
		os.Exit(1)
	}

	collector := cgroup.NewCollector(cgroup.Config{
		NodeName:     cfg.NodeName,
		PollInterval: cfg.CgroupPollInterval,
		CgroupRoot:   "/sys/fs/cgroup",
		ProcPath:     "/proc",
		SysPath:      "/sys",
	}, podStore, criClient, log)

	prometheus.MustRegister(collector)
	go collector.Run(ctx)

	log.Info("cgroup: subsystem started")
}

func startEBPF(ctx context.Context, cfg *config.Config, stores informerStores, log *slog.Logger) {
	resolver := device.NewResolver(
		cfg.NodeName,
		cfg.EBPFProcPath,
		time.Duration(cfg.EBPFScanInterval)*time.Second,
		stores.podStore,
		stores.pvcIndexer,
		log,
	)
	go resolver.Run(ctx)

	programs, err := bpf.LoadAndAttach(
		cfg.EnableEBPFBlock, cfg.EnableEBPFNFS, cfg.EnableEBPFNFSKprobe,
		cfg.EBPFBlockMapSize, cfg.EBPFNFSMapSize, cfg.EBPFNFSKprobeMapSize,
		log,
	)
	if err != nil {
		log.Warn("ebpf: failed to load programs, eBPF monitoring disabled", "error", err)
		return
	}

	go programs.RetryFailed(ctx, 1*time.Minute, log)

	go func() {
		<-ctx.Done()
		programs.Close()
	}()

	collector := bpf.NewCollector(programs, resolver, cfg.NodeName, cfg.Boundaries, config.ParseNamespaces(cfg.Namespaces), log)
	prometheus.MustRegister(collector)

	log.Info("ebpf: subsystem started",
		"block", programs.BlockActive,
		"nfs", programs.NFSActive,
		"nfsKprobe", programs.NFSKprobeActive,
	)
}

func setupLogging(level string) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}

func healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	return mux
}

func metricsHandler(includeHealth bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	if includeHealth {
		mux.Handle("/healthz", healthHandler())
	}
	return mux
}
