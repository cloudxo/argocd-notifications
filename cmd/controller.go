package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/argoproj-labs/argocd-notifications/controller"
	"github.com/argoproj-labs/argocd-notifications/pkg/services"
	"github.com/argoproj-labs/argocd-notifications/shared/argocd"
	"github.com/argoproj-labs/argocd-notifications/shared/cmd"
	"github.com/argoproj-labs/argocd-notifications/shared/k8s"
	"github.com/argoproj-labs/argocd-notifications/shared/settings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultMetricsPort = 9001
)

func newControllerCommand() *cobra.Command {
	var (
		clientConfig     clientcmd.ClientConfig
		processorsCount  int
		namespace        string
		appLabelSelector string
		logLevel         string
		metricsPort      int
		argocdRepoServer string
	)
	var command = cobra.Command{
		Use: "controller",
		RunE: func(c *cobra.Command, args []string) error {
			restConfig, err := clientConfig.ClientConfig()
			if err != nil {
				return err
			}
			dynamicClient, err := dynamic.NewForConfig(restConfig)
			if err != nil {
				return err
			}
			k8sClient, err := kubernetes.NewForConfig(restConfig)
			if err != nil {
				return err
			}
			if namespace == "" {
				namespace, _, err = clientConfig.Namespace()
				if err != nil {
					return err
				}
			}
			level, err := log.ParseLevel(logLevel)
			if err != nil {
				return err
			}
			log.SetLevel(level)

			argocdService, err := argocd.NewArgoCDService(k8sClient, namespace, argocdRepoServer)
			if err != nil {
				return err
			}
			defer argocdService.Close()

			registry := controller.NewMetricsRegistry()
			http.Handle("/metrics", promhttp.HandlerFor(prometheus.Gatherers{registry, prometheus.DefaultGatherer}, promhttp.HandlerOpts{}))

			go func() {
				log.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", metricsPort), http.DefaultServeMux))
			}()
			log.Infof("serving metrics on port %d", metricsPort)
			log.Infof("loading configuration %d", metricsPort)

			var cancelPrev context.CancelFunc
			watchConfig(context.Background(), argocdService, k8sClient, namespace, func(cfg settings.Config) error {
				if cancelPrev != nil {
					log.Info("Settings had been updated. Restarting controller...")
					cancelPrev()
					cancelPrev = nil
				}
				ctrl, err := controller.NewController(dynamicClient, namespace, cfg, appLabelSelector, registry)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancelPrev = cancel

				err = ctrl.Init(ctx)
				if err != nil {
					return err
				}

				go ctrl.Run(ctx, processorsCount)
				return nil
			})
			<-context.Background().Done()
			return nil
		},
	}
	clientConfig = cmd.AddK8SFlagsToCmd(&command)
	command.Flags().IntVar(&processorsCount, "processors-count", 1, "Processors count.")
	command.Flags().StringVar(&appLabelSelector, "app-label-selector", "", "App label selector.")
	command.Flags().StringVar(&namespace, "namespace", "", "Namespace which controller handles. Current namespace if empty.")
	command.Flags().StringVar(&logLevel, "loglevel", "info", "Set the logging level. One of: debug|info|warn|error")
	command.Flags().IntVar(&metricsPort, "metrics-port", defaultMetricsPort, "Metrics port")
	command.Flags().StringVar(&argocdRepoServer, "argocd-repo-server", "argocd-repo-server:8081", "Argo CD repo server address")
	return &command
}

func watchConfig(ctx context.Context, argocdService argocd.Service, clientset kubernetes.Interface, namespace string, callback func(settings.Config) error) {
	var secret *v1.Secret
	var configMap *v1.ConfigMap
	lock := &sync.Mutex{}
	onNewConfigMapAndSecret := func(newSecret *v1.Secret, newConfigMap *v1.ConfigMap) {
		lock.Lock()
		defer lock.Unlock()
		if newSecret != nil {
			secret = newSecret
		}
		if newConfigMap != nil {
			configMap = newConfigMap
		}

		if secret != nil && configMap != nil {
			if cfg, err := settings.NewConfig(configMap, secret, argocdService); err == nil {
				// add console service that is useful for debugging
				cfg.Notifier.AddService("console", services.NewConsoleService(os.Stdout))

				if err = callback(*cfg); err != nil {
					log.Fatalf("Failed to start controller: %v", err)
				}
			} else {
				log.Fatalf("Failed to parse new settings: %v", err)
			}
		}
	}

	onConfigMapChanged := func(newObj interface{}) {
		if cm, ok := newObj.(*v1.ConfigMap); ok {
			onNewConfigMapAndSecret(nil, cm)
		}
	}

	onSecretChanged := func(newObj interface{}) {
		if s, ok := newObj.(*v1.Secret); ok {
			onNewConfigMapAndSecret(s, nil)
		}
	}

	cmInformer := k8s.NewConfigMapInformer(clientset, namespace)
	cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			onConfigMapChanged(newObj)
		},
		AddFunc: func(obj interface{}) {
			log.Info("config map found")
			onConfigMapChanged(obj)
		},
	})

	secretInformer := k8s.NewSecretInformer(clientset, namespace)
	secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			onSecretChanged(newObj)
		},
		AddFunc: func(obj interface{}) {
			log.Info("secret found")
			onSecretChanged(obj)
		},
	})
	go secretInformer.Run(ctx.Done())
	go cmInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), cmInformer.HasSynced, secretInformer.HasSynced) {
		log.Fatal(errors.New("timed out waiting for caches to sync"))
	}
	var missingWarn []string
	if len(cmInformer.GetStore().List()) == 0 {
		missingWarn = append(missingWarn, fmt.Sprintf("config map %s", k8s.ConfigMapName))
	}
	if len(secretInformer.GetStore().List()) == 0 {
		missingWarn = append(missingWarn, fmt.Sprintf("secret %s", k8s.SecretName))
	}
	if len(missingWarn) > 0 {
		log.Warnf("Cannot find %s. Waiting when both config map and secret are created.", strings.Join(missingWarn, " and "))
	}
}