package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"github.com/alecthomas/kong"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/v8/controller"
)

func registerSignalShutdown(shutdownFunc context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		logrus.Infof("Receive %v to exit", sig)
		shutdownFunc()
	}()
}

const (
	VERSION = "0.0.1"
)

func kongDefaultVars() kong.Vars {
	return kong.Vars{
		"WorkerThreads":       fmt.Sprintf("%d", pvController.DefaultThreadiness),
		"ProvisionRetryCount": fmt.Sprintf("%d", pvController.DefaultFailedProvisionThreshold),
		"DeletionRetryCount":  fmt.Sprintf("%d", pvController.DefaultFailedDeleteThreshold),
	}
}

type StartCmd struct {
	LocalConfig            string `env:"LOCAL_CONFIG" help:"local YAML file to use rather than reading data from configmap.  Primarily for testing"`
	ProvisionerName        string `env:"PROVISIONER_NAME" default:"rancher.io/local-path"`
	Namespace              string `env:"POD_NAMESPACE"  default:"local-path-storage" help:"The namespace that Provisioner is running in"`
	KubeConfig             string `name:"kubeconfig" help:"Paths to a kubeconfig. Only required when it is out-of-cluster."`
	ConfigmapName          string `default:"local-path-config" help:"Specify configmap name."`
	WorkerThreads          uint16 `default:"${WorkerThreads}" help:"Number of provisioner worker threads."`
	ProvisioningRetryCount uint32 `default:"${ProvisionRetryCount}" help:"Number of retries of failed volume provisioning. 0 means retry indefinitely."`
	DeletionRetryCount     uint32 `default:"${DeletionRetryCount}"  help:"Number of retries of failed volume deletion. 0 means retry indefinitely."`
}

func (s *StartCmd) Run() (err error) {
	ctx, shutdownFunc := context.WithCancel(context.Background())
	registerSignalShutdown(shutdownFunc)

	config := new(Config)
	if s.LocalConfig != "" {
		logrus.Infof("attempting to load local ConfigMap data from %s", s.LocalConfig)

		contents, err := ioutil.ReadFile(s.LocalConfig)
		err = errors.Wrap(err, "while loading local ConfigMap data")
		if err == nil {
			err = errors.Wrap(strictYAMLUnmarshal(contents, config), "while deserializing JSON")
		}
		if err != nil {
			return err
		}
		logrus.Infof("loaded config: %s", *config)
	}

	kubeConfig, err := loadConfig(s.KubeConfig)
	if err != nil {
		return errors.Wrap(err, "unable to get client config")
	}

	// TODO: rewrite this to use https://pkg.go.dev/k8s.io/client-go/tools/clientcmd
	kubeClient, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	if config == nil {
		panic("config must be passed")
	}
	provisioner, err := NewProvisioner(ctx, kubeClient, config, s.Namespace, s.ConfigmapName)
	if err != nil {
		return err
	}
	pc := pvController.NewProvisionController(
		kubeClient,
		s.ProvisionerName,
		provisioner,
		pvController.LeaderElection(false),
		pvController.FailedProvisionThreshold(int(s.ProvisioningRetryCount)),
		pvController.FailedDeleteThreshold(int(s.DeletionRetryCount)),
		pvController.Threadiness(int(s.WorkerThreads)),
	)
	logrus.Debug("Provisioner started")
	pc.Run(ctx)
	logrus.Debug("Provisioner stopped")
	return nil
}

type debugFlag bool

func (d debugFlag) BeforeApply() error {
	logrus.SetLevel(logrus.DebugLevel)
	return nil
}

type CLI struct {
	Debug debugFlag `short:"d" help:"Enable debug logging level" env:"RANCHER_DEBUG"`
	Start StartCmd  `cmd:"" help:"Run a provisioner instance"`
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if len(kubeconfig) > 0 {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	kubeconfigPath := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)
	if len(kubeconfigPath) > 0 {
		envVarFiles := filepath.SplitList(kubeconfigPath)
		for _, f := range envVarFiles {
			if _, err := os.Stat(f); err == nil {
				return clientcmd.BuildConfigFromFlags("", f)
			}
		}
	}

	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}

	kubeconfig = filepath.Join(homeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func findConfigFileFromConfigMap(ctx context.Context, kubeClient clientset.Interface, namespace, configMapName, key string) (string, error) {
	cm, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	value, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("%v is not exist in local-path-config ConfigMap", key)
	}
	return value, nil
}

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	///var cli CLI
	var cli CLI
	ctx := kong.Parse(&cli, kongDefaultVars())

	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
