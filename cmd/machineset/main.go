/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	osconfigv1 "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/library-go/pkg/config/leaderelection"
	"github.com/openshift/machine-api-operator/pkg/controller"
	"github.com/openshift/machine-api-operator/pkg/controller/machineset"
	"github.com/openshift/machine-api-operator/pkg/metrics"
	"github.com/openshift/machine-api-operator/pkg/operator"
	"github.com/openshift/machine-api-operator/pkg/util"
	mapiwebhooks "github.com/openshift/machine-api-operator/pkg/webhooks"
)

const (
	defaultWebhookPort    = operator.MachineSetWebhookPort
	defaultWebhookCertdir = "/etc/machine-api-operator/tls"
)

func main() {
	// Used to get the default values for leader election from library-go
	defaultLeaderElectionValues := leaderelection.LeaderElectionDefaulting(
		osconfigv1.LeaderElection{},
		"", "",
	)

	klog.InitFlags(nil)
	if err := flag.Set("logtostderr", "true"); err != nil {
		klog.Fatalf("failed to set logtostderr flag: %v", err)
	}
	watchNamespace := flag.String("namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")
	metricsAddress := flag.String("metrics-bind-address", metrics.DefaultMachineSetMetricsAddress, "Address for hosting metrics")

	webhookEnabled := flag.Bool("webhook-enabled", true,
		"Webhook server, enabled by default. When enabled, the manager will run a webhook server.")

	webhookPort := flag.Int("webhook-port", defaultWebhookPort,
		"Webhook Server port, only used when webhook-enabled is true.")

	webhookCertdir := flag.String("webhook-cert-dir", defaultWebhookCertdir,
		"Webhook cert dir, only used when webhook-enabled is true.")

	healthAddr := flag.String(
		"health-addr",
		":9441",
		"The address for health checking.",
	)

	leaderElectResourceNamespace := flag.String(
		"leader-elect-resource-namespace",
		"",
		"The namespace of resource object that is used for locking during leader election. If unspecified and running in cluster, defaults to the service account namespace for the controller. Required for leader-election outside of a cluster.",
	)

	leaderElect := flag.Bool(
		"leader-elect",
		false,
		"Start a leader election client and gain leadership before executing the main loop. Enable this when running replicated components for high availability.",
	)

	// Default values are printed for the user to see, but zero is set as the default to distinguish user intent from default value for topology aware leader election
	leaderElectLeaseDuration := flag.Duration(
		"leader-elect-lease-duration",
		0,
		fmt.Sprintf("The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped before it is replaced by another candidate. This is only applicable if leader election is enabled. Default: (%s)", defaultLeaderElectionValues.LeaseDuration.Duration),
	)

	flag.Parse()
	if *watchNamespace != "" {
		log.Printf("Watching cluster-api objects only in namespace %q for reconciliation.", *watchNamespace)
	}

	log.Printf("Registering Components.")
	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatal(err)
	}

	le := util.GetLeaderElectionConfig(cfg, osconfigv1.LeaderElection{
		Disable:       !*leaderElect,
		LeaseDuration: metav1.Duration{Duration: *leaderElectLeaseDuration},
	})

	// Create a new Cmd to provide shared dependencies and start components
	syncPeriod := 10 * time.Minute
	opts := manager.Options{
		MetricsBindAddress:      *metricsAddress,
		SyncPeriod:              &syncPeriod,
		Namespace:               *watchNamespace,
		HealthProbeBindAddress:  *healthAddr,
		LeaderElection:          *leaderElect,
		LeaderElectionNamespace: *leaderElectResourceNamespace,
		LeaderElectionID:        "cluster-api-provider-machineset-leader",
		LeaseDuration:           &le.LeaseDuration.Duration,
		RetryPeriod:             &le.RetryPeriod.Duration,
		RenewDeadline:           &le.RenewDeadline.Duration,
	}

	mgr, err := manager.New(cfg, opts)
	if err != nil {
		log.Fatal(err)
	}

	// Enable defaulting and validating webhooks
	machineDefaulter, err := mapiwebhooks.NewMachineDefaulter()
	if err != nil {
		log.Fatal(err)
	}

	machineValidator, err := mapiwebhooks.NewMachineValidator(mgr.GetClient())
	if err != nil {
		log.Fatal(err)
	}

	machineSetDefaulter, err := mapiwebhooks.NewMachineSetDefaulter()
	if err != nil {
		log.Fatal(err)
	}

	machineSetValidator, err := mapiwebhooks.NewMachineSetValidator(mgr.GetClient())
	if err != nil {
		log.Fatal(err)
	}

	if *webhookEnabled {
		mgr.GetWebhookServer().Port = *webhookPort
		mgr.GetWebhookServer().CertDir = *webhookCertdir
		mgr.GetWebhookServer().Register(mapiwebhooks.DefaultMachineMutatingHookPath, &webhook.Admission{Handler: machineDefaulter})
		mgr.GetWebhookServer().Register(mapiwebhooks.DefaultMachineValidatingHookPath, &webhook.Admission{Handler: machineValidator})
		mgr.GetWebhookServer().Register(mapiwebhooks.DefaultMachineSetMutatingHookPath, &webhook.Admission{Handler: machineSetDefaulter})
		mgr.GetWebhookServer().Register(mapiwebhooks.DefaultMachineSetValidatingHookPath, &webhook.Admission{Handler: machineSetValidator})
	}

	log.Printf("Registering Components.")

	// Setup Scheme for all resources
	if err := machinev1.AddToScheme(mgr.GetScheme()); err != nil {
		log.Fatal(err)
	}

	// Setup all Controllers
	if err := controller.AddToManager(mgr, opts, machineset.Add); err != nil {
		log.Fatal(err)
	}

	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		klog.Fatal(err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.Fatal(err)
	}

	log.Printf("Starting the Cmd.")

	// Start the Cmd
	log.Fatal(mgr.Start(signals.SetupSignalHandler()))
}
