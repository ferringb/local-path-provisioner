package main

import (
	"context"
	"fmt"
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

func RegisterSignalShutdown(shutdownFunc context.CancelFunc) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		logrus.Infof("Receive %v to exit", sig)
		shutdownFunc()
	}()
}

const (
	VERSION              = "0.0.1"
	DefaultConfigFileKey = "config.json"
	DefaultHelperPodFile = "helperPod.yaml"
)

func kongDefaultVars() kong.Vars {
	return kong.Vars{
		"WorkerThreads":       fmt.Sprintf("%d", pvController.DefaultThreadiness),
		"ProvisionRetryCount": fmt.Sprintf("%d", pvController.DefaultFailedProvisionThreshold),
		"DeletionRetryCount":  fmt.Sprintf("%d", pvController.DefaultFailedDeleteThreshold),
	}
}

type StartCmd struct {
	Config                 string `default:"config.json" help:"Provisioner config file. If provided, overrides any configs loaded via --config-map-name"`
	ProvisionerName        string `env:"PROVISIONER_NAME" default:"rancher.io/local-path"`
	Namespace              string `env:"POD_NAMESPACE"  default:"local-path-storage" help:"The namespace that Provisioner is running in"`
	KubeConfig             string `name:"kubeconfig" help:"Paths to a kubeconfig. Only required when it is out-of-cluster."`
	ConfigmapName          string `default:"local-path-config" help:"Specify configmap name."`
	HelperPodFile          string `help:"Paths to the Helper pod yaml file"`
	WorkerThreads          uint16 `default:"${WorkerThreads}" help:"Number of provisioner worker threads."`
	ProvisioningRetryCount uint32 `default:"${ProvisionRetryCount}" help:"Number of retries of failed volume provisioning. 0 means retry indefinitely."`
	DeletionRetryCount     uint32 `default:"${DeletionRetryCount}"  help:"Number of retries of failed volume deletion. 0 means retry indefinitely."`
}

func (s *StartCmd) Run() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("Error starting daemon: %w", err)

		}
	}()
	ctx, shutdownFunc := context.WithCancel(context.Background())
	RegisterSignalShutdown(shutdownFunc)

	config, err := loadConfig(s.KubeConfig)
	if err != nil {
		return errors.Wrap(err, "unable to get client config")
	}

	kubeClient, err := clientset.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	configFile := s.Config
	if configFile == "" {
		configFile, err = findConfigFileFromConfigMap(ctx, kubeClient, s.Namespace, s.ConfigmapName, DefaultConfigFileKey)
		if err != nil {
			return fmt.Errorf("failed to load config file from ConfigMap %v/%v key %v; either add that value, or provide via a local file.  Error was: %v", s.Namespace, s.ConfigmapName, DefaultConfigFileKey, err)
		}
	}

	// if helper pod file is not specified, then find the helper pod by configmap with key = helperPod.yaml
	// if helper pod file is specified with flag FlagHelperPodFile, then load the file
	helperPodFile := s.HelperPodFile
	helperPodYaml := ""
	if helperPodFile == "" {
		helperPodYaml, err = findConfigFileFromConfigMap(ctx, kubeClient, s.Namespace, s.ConfigmapName, DefaultHelperPodFile)
		if err != nil {
			return fmt.Errorf("failed to load helpoer pod definition from %v/%v key %v; either add that value, or provide it via a local command line flag.  Error was: %v", s.Namespace, s.ConfigmapName, DefaultHelperPodFile, err)
		}
	} else {
		helperPodYaml, err = loadFile(helperPodFile)
		if err != nil {
			return fmt.Errorf("could not open file %v with err: %v", helperPodFile, err)
		}
	}

	provisioner, err := NewProvisioner(ctx, kubeClient, configFile, s.Namespace, s.ConfigmapName, helperPodYaml)
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
