package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

/*
// OldNodePathEntry is used for deserializing the old config format describing nodepaths
type OldNodePathEntry struct {
	Node  string   `json:"node,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// OldConfigLayout represents and is used to deserialize the original config used prior to ~2022-06
type OldConfigLayout struct {
	ConfigJson struct {
		NodePathsList     []*OldNodePathEntry `json:"nodePathMap"`
		CmdTimeoutSeconds int                 `json:"cmdTimeoutSeconds,omitempty`
	} `json:"config.json"`

	SetupCmd      string `json:"setup"`
	TeardownCmd   string `json:"teardown"`
	HelperPodYaml string `json:"helperPod.yaml"`
}
*/

const (
	DEFAULT_CMD_TIMEOUT = time.Second * 120
	MAX_CMD_TIMEOUT     = time.Hour * 24
)

type NodeConfig struct {
	Path            string        `json:"path,omitempty"`
	HelperPodYaml   string        `json:"helperPod.yaml,omitempty"`
	Setup           string        `json:"setup,omitempty"`
	SetupTimeout    time.Duration `json:"setupTimeout,omitempty"`
	Teardown        string        `json:"teardown,omitempty"`
	TeardownTimeout time.Duration `json:"teardownTimeout,omitempty"`
}

func (nc NodeConfig) String() string {
	return jsonStringer(nc)
}

func (nc *NodeConfig) UnmarshalJSON(data []byte) error {
	type _NodeConfig NodeConfig
	if err := strictJSONUnmarshal(data, (*_NodeConfig)(nc)); err != nil {
		return err
	}
	return nc.CheckValues()
}

func (nc *NodeConfig) CheckValues() error {
	var result *multierror.Error
	if nc.Path != "" {
		var err error
		p, err := filepath.Abs(nc.Path)
		if err != nil && p == "/" {
			err = errors.New("path must not be empty or /")
		}
		if err == nil {
			nc.Path = p
		} else {
			result = multierror.Append(result, errors.Wrap(err, "while parsing path"))
		}
	}
	if nc.HelperPodYaml != "" {
		// cache this result once abstractions aren't as screwed up
		_, err := nc.TryGetHelperPod()
		result = multierror.Append(result, errors.Wrap(err, "while parsing helperPod.yaml"))
	}
	checkDuration := func(field string, value time.Duration) error {
		if value >= MAX_CMD_TIMEOUT {
			return fmt.Errorf("%s can't exceed %s; got %s", field, MAX_CMD_TIMEOUT, value)
		} else if value < 0 {
			return fmt.Errorf("%s must be positive; got %s", field, value)
		}
		return nil
	}
	result = multierror.Append(result, checkDuration("setup timeout", nc.SetupTimeout))
	result = multierror.Append(result, checkDuration("teardown timeout", nc.TeardownTimeout))
	return result.ErrorOrNil()
}

func (nc *NodeConfig) TryGetHelperPod() (*v1.Pod, error) {
	// implement memoization/caching for this at some point
	p := v1.Pod{}
	err := yaml.Unmarshal([]byte(nc.HelperPodYaml), &p)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal HelperPod definition: %v caused %w", nc.HelperPodYaml, err)
	}
	if len(p.Spec.Containers) == 0 {
		return nil, fmt.Errorf("helper pod template does not specify any container")
	}
	return &p, nil
}

func (nc *NodeConfig) GetHelperPod() *v1.Pod {
	result, err := nc.TryGetHelperPod()
	if err != nil {
		panic(err)
	}
	return result
}

func (nc *NodeConfig) combine(other *NodeConfig) *NodeConfig {
	newNode := NodeConfig(*nc)
	if newNode.Path == "" {
		newNode.Path = other.Path
	}
	if newNode.HelperPodYaml == "" {
		newNode.HelperPodYaml = other.HelperPodYaml
	}
	if newNode.Setup == "" {
		newNode.Setup = other.Setup
	}
	if newNode.SetupTimeout == 0 {
		newNode.SetupTimeout = other.SetupTimeout
	}
	if newNode.Teardown == "" {
		newNode.Teardown = other.Teardown
	}
	if newNode.TeardownTimeout == 0 {
		newNode.TeardownTimeout = other.TeardownTimeout
	}
	return &newNode
}

func (nc *NodeConfig) IsComplete() error {
	var result *multierror.Error

	nc_type := reflect.TypeOf(*nc)
	nc_values := reflect.ValueOf(*nc)
	for i := 0; i < nc_values.NumField(); i++ {
		field := nc_values.Field(i)
		if field.IsZero() {
			result = multierror.Append(result, fmt.Errorf("field %s must be defined, got '%s'", nc_type.Field(i).Name, field.Interface()))
		}
	}
	return result.ErrorOrNil()
}

func (nc *NodeConfig) IsUsable() error {
	return multierror.Append(nc.CheckValues(), nc.IsComplete()).ErrorOrNil()
}

type Config struct {
	NodeOverrides map[string]NodeConfig `json:"nodeOverrides,omitempty"`
	// embedded defaults
	NodeConfig `json:",inline"`
}

func (c Config) String() string {
	return jsonStringer(c)
}

func (c *Config) UnmarshalJSON(b []byte) error {
	type _Config Config

	c.SetupTimeout = DEFAULT_CMD_TIMEOUT
	c.TeardownTimeout = DEFAULT_CMD_TIMEOUT

	if err := strictJSONUnmarshal(b, (*_Config)(c)); err != nil {
		return err
	}
	return c.IsComplete()
}

// GetNodeConfig returns the node config to use for a given node name.  This will apply all overrides
// defined in the config, and fallback to default values as needed.
func (c *Config) GetNodeConfig(nodeName string) *NodeConfig {
	if node, ok := c.NodeOverrides[nodeName]; ok {
		return node.combine(&c.NodeConfig)
	}
	node := NodeConfig(c.NodeConfig)
	return &node

}

/*
func FindConfigFileFromConfigMap(ctx context.Context, client kubernetes.Interface, namespace, configMapName,
	key string) (chan<- *Config, error) {

	updates := make(chan *Config)
	watcher := informer.NewInformedWatcher(client, namespace)
	watcher.Watch(configMapName, func(updated *corev1.ConfigMap) {
		// be lazy about it; serialize the config data into json and back to get the struct we want.
		data, err := json.Marshal(updated.Data)
		if err != nil {
			logrus.Errorf("configmap was updated, but failed to marshal it to json: %v", err)
			return
		}
		var config Config
		if err = json.Unmarshal(data, &config); err != nil {
			logrus.Errorf("configmap was updated, but failed to unmarshal it from json", err)
			return
		}
		logrus.Info("config.json unmarshalled.  Update being processed")
		updates <- &config
	})
	watcher.Start(ctx.Done())
	return updates, nil
}
*/
