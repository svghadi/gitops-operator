package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/redhat-developer/gitops-operator/internal/ai/trigger"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var commit = "unknown"

func main() {
	var addr string
	var tlsCert string
	var tlsKey string
	var tlsEnabled bool

	flag.StringVar(&addr, "addr", ":8443", "Listen address")
	flag.StringVar(&tlsCert, "tls-cert", "/etc/agentic-trigger/tls/tls.crt", "TLS certificate path")
	flag.StringVar(&tlsKey, "tls-key", "/etc/agentic-trigger/tls/tls.key", "TLS key path")
	flag.BoolVar(&tlsEnabled, "tls-enabled", true, "Enable TLS (set false for plain HTTP)")
	flag.Parse()

	log.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := log.Log.WithName("agentic-trigger")
	logger.Info("starting", "commit", commit)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "failed to get in-cluster config")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "failed to create kubernetes client")
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "failed to create dynamic client")
		os.Exit(1)
	}

	config, err := trigger.LoadServiceConfig()
	if err != nil {
		logger.Error(err, "invalid trigger configuration")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	trigger.New(config, kubeClient, dynClient, logger).Run(ctx, addr, tlsCert, tlsKey, tlsEnabled)
}
