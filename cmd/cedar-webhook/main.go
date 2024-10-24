package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cedar-policy/cedar-go"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/authorization/union"
	"k8s.io/component-base/cli"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/featuregate"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	cradmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/awslabs/cedar-access-control-for-k8s/internal/server"
	"github.com/awslabs/cedar-access-control-for-k8s/internal/server/admission"
	cedarauthorizer "github.com/awslabs/cedar-access-control-for-k8s/internal/server/authorizer"
	"github.com/awslabs/cedar-access-control-for-k8s/internal/server/config"
	serveroptions "github.com/awslabs/cedar-access-control-for-k8s/internal/server/options"
	"github.com/awslabs/cedar-access-control-for-k8s/internal/server/store"
)

var (
	featureGate = featuregate.NewFeatureGate()
)

func main() {
	runtime.Must(logsapi.AddFeatureGates(featureGate))
	command := NewAuthorizerCommand()
	os.Exit(cli.Run(command))
}

// NewAuthorizerCommand creates a new cobra.Command which is used to start the
// authenticator server.
func NewAuthorizerCommand() *cobra.Command {
	c := logsapi.NewLoggingConfiguration()
	o := serveroptions.NewCedarAuthorizerOptions()
	cmd := &cobra.Command{
		Use: "cedar-webhook",
		Long: `The cedar-webhook is an authorization and admission webhook 
		server which makes decisions based on cedar policy configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := logsapi.ValidateAndApply(c, featureGate); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			verflag.PrintAndExitIfRequested()
			cliflag.PrintFlags(cmd.Flags())

			config, err := o.Config()
			if err != nil {
				return err
			}

			return Run(config)
		},
	}

	fs := cmd.Flags()
	namedFlagSets := o.Flags()

	// Use kubernetes CLI helpers
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	featureGate.AddFlag(namedFlagSets.FlagSet("global"))
	logsapi.AddFlags(c, namedFlagSets.FlagSet("global"))
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, *namedFlagSets, cols)

	return cmd
}

// Run starts the authorizer server
func Run(config *config.AuthorizationWebhookConfig) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel() }()
	klog.InfoS("Starting cedar-webhook", "version", version.Get())

	// TODO: use a unique policy name to not collide with other policies
	crdStore, err := store.NewCRDPolicyStore(cedar.PolicyMap{
		"always-allow": admission.AllowAllAdmissionPolicy(),
	})
	if err != nil {
		klog.Fatal("error creating CRD policy store", "error", err)
	}

	cedarauthorizer.NewAuthorizer(crdStore)
	authorizer := union.New(
		// file-backed authorizer goes first
		cedarauthorizer.NewAuthorizer(store.NewLocalPolicyStore(config.PolicyDir, config.PolicyDirRefreshInterval)),
		// CRD-backed authorizer goes last
		cedarauthorizer.NewAuthorizer(crdStore),
	)

	// TODO support multiple policy stores for admission
	vWebhook := &cradmission.Webhook{Handler: admission.NewCedarHandler(crdStore, true)}

	srv := server.NewServer(authorizer, vWebhook, config)
	serverShutdownCh, listenerStoppedCh, err := config.SecureServing.Serve(srv.GetHandler(), 0, server.DeriveStopChannel(ctx))

	if err != nil {
		return err
	}

	go func() {
		s := server.NewMetricsServer()
		if err := s.ListenAndServe(); err != nil {
			klog.ErrorS(err, "Failed to start metrics server")
			// If we fail to set up metrics then shutdown the server
			cancel()
		}
	}()

	<-serverShutdownCh
	klog.InfoS("Server shutting down")

	<-listenerStoppedCh
	klog.InfoS("Server stopped listening")

	return nil
}