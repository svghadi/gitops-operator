package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redhat-developer/gitops-operator/internal/agentic"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

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

	config, err := loadServiceConfig()
	if err != nil {
		logger.Error(err, "invalid trigger configuration")
		os.Exit(1)
	}

	svc := &TriggerService{
		kubeClient: kubeClient,
		dynClient:  dynClient,
		config:     config,
		logger:     logger,
		queue:      newDiagnosisQueue(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/diagnose", svc.HandleDiagnose)
	mux.HandleFunc("GET /api/v1/applications/{namespace}/{name}/diagnosis", svc.HandleDiagnosis)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if tlsEnabled {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		svc.runWorker(ctx)
	}()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		svc.runAgenticRunEvents(ctx)
	}()

	go func() {
		if tlsEnabled {
			logger.Info("starting agentic-trigger service with TLS", "addr", addr)
			if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != nil && err != http.ErrServerClosed {
				logger.Error(err, "server error")
				os.Exit(1)
			}
		} else {
			logger.Info("starting agentic-trigger service without TLS (plain HTTP)", "addr", addr)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error(err, "server error")
				os.Exit(1)
			}
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	<-watcherDone
	<-workerDone
}

func loadServiceConfig() (ServiceConfig, error) {
	cooldown, err := agentic.Duration(os.Getenv("AGENTIC_RUN_COOLDOWN"), 10*time.Minute, false)
	if err != nil {
		return ServiceConfig{}, err
	}
	autoDiagnosisGracePeriod, err := agentic.Duration(os.Getenv("AGENTIC_AUTO_DIAGNOSIS_GRACE_PERIOD"), 10*time.Minute, true)
	if err != nil {
		return ServiceConfig{}, err
	}

	namespace := os.Getenv("AGENTIC_RUN_NAMESPACE")
	if namespace == "" {
		namespace = "openshift-lightspeed"
	}

	agent := os.Getenv("AGENTIC_ANALYSIS_AGENT")
	if agent == "" {
		agent = "default"
	}

	skillsImage := os.Getenv("AGENTIC_SKILLS_IMAGE")

	argocdNamespace := os.Getenv("ARGOCD_NAMESPACE")
	if argocdNamespace == "" {
		argocdNamespace = "openshift-gitops"
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		argocdNamespace = strings.TrimSpace(string(data))
	}

	return ServiceConfig{
		RunNamespace:             namespace,
		AnalysisAgent:            agent,
		Cooldown:                 cooldown,
		SkillsImage:              skillsImage,
		ArgocdNamespace:          argocdNamespace,
		AutoDiagnosisGracePeriod: autoDiagnosisGracePeriod,
	}, nil
}

type ServiceConfig struct {
	RunNamespace             string
	AnalysisAgent            string
	Cooldown                 time.Duration
	SkillsImage              string
	ArgocdNamespace          string
	AutoDiagnosisGracePeriod time.Duration
}
