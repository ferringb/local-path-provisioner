package main

import (
	"fmt"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

const MinimalNodeConfigYaml = `
setup: setup-script
teardown: teardown-script
path: /test-path
helperPod.yaml: |-
  apiVersion: v1
  kind: Pod
  metadata:
    name: helper-pod
  spec:
    containers:
    - name: helper-pod
      image: busybox

`

// Note: configs are in yaml for human and test readability.  It still resolves down to json internally,
// thus executing our intended pathways.

func deserialize[T any, D string | []byte](yamlSource D, patches ...jsonpatch.Patch) (*T, error) {

	jsonData, err := yaml.YAMLToJSONStrict([]byte(yamlSource))
	if err != nil {
		return nil, err
	}
	// correct a yaml -> json difference; yaml.Unmarshal("") yields "", which json doesn't
	// view as an object; effectively it bypasses the actual parsing.
	if len(jsonData) == 0 {
		jsonData = []byte("{}")
	}

	for _, patch := range patches {
		if jsonData, err = patch.Apply(jsonData); err != nil {
			panic(
				fmt.Errorf("failed applying patch %v\n to\n%v\nerror was: %w", patch, jsonData, err))
		}
	}
	obj := new(T)
	if serr, err := json.UnmarshalStrict(jsonData, obj); err != nil || serr != nil {
		if serr != nil {
			return nil, multierror.Append(serr[0], serr...)
		}
		return nil, err
	}
	return obj, nil
}

func deserializeObj[T any, D string | []byte](t *testing.T, yamlSource D, msg string, patches ...jsonpatch.Patch) *T {
	obj, err := deserialize[T](yamlSource, patches...)
	// inject the err into the message since require doesn't use %s
	require.Nil(t, err, fmt.Sprintf("%s\nexplicit err: %s", msg, err))
	return obj
}

func deserializeErr[T any, D string | []byte](t *testing.T, yamlSource D, msg string, patches ...jsonpatch.Patch) error {
	obj, err := deserialize[T](yamlSource, patches...)
	require.Error(t, err, fmt.Sprintf("%s:\nunexpected obj was %v", msg, obj))
	return err
}

func patch[D string | []byte](raw D) jsonpatch.Patch {
	p, err := jsonpatch.DecodePatch([]byte(raw))
	if err != nil {
		panic(fmt.Sprintf("patch failed to be created: was given %v, error was %v", raw, err))
	}
	return p
}

func TestNodeConfigCombine(t *testing.T) {
	// verify it detects missing fields.
	src := deserializeObj[NodeConfig](t, MinimalNodeConfigYaml, "minimal yaml must parse")
	src.SetupTimeout = 1234 * time.Second
	src.TeardownTimeout = 2345 * time.Second
	empty := &NodeConfig{}
	n := empty.combine(src)
	require.Equal(t, *src, *n, "empty node must be the same as src")
	empty.SetupTimeout = 5 * time.Second
	require.Equal(t, 5*time.Second, empty.combine(src).SetupTimeout, "overrides must be preserved, SetupTimeout wasn't")
}

func TestNodeConfigParsing(t *testing.T) {
	t.Parallel()

	{
		obj := deserializeObj[NodeConfig](t, MinimalNodeConfigYaml, "minimal yaml must parse")
		require.Equal(t, obj.Setup, "setup-script")
		require.Equal(t, obj.Teardown, "teardown-script")
		require.Equal(t, obj.Path, "/test-path")
	}

	// verify it pukes on unknown fields
	deserializeErr[NodeConfig](
		t,
		MinimalNodeConfigYaml,
		"NodeConfig: unknown fields should be rejected",
		patch(`[{"op":"replace", "path":"/asdf", "value":1}]`),
	)
	deserializeErr[NodeConfig](
		t,
		MinimalNodeConfigYaml,
		"unknown fields should be rejected",
		patch(`[{"op":"replace", "path":"/asdf", "value":1}]`),
	)

	// verify integer parsing and >=0 values
	for _, field := range []string{"setupTimeout", "teardownTimeout"} {
		deserializeErr[NodeConfig](
			t,
			MinimalNodeConfigYaml,
			fmt.Sprintf("field %s must require integer parsing", field),
			patch(fmt.Sprintf(`[{"op":"replace", "path":"/%s", "value":"asdf"}]`, field)),
		)
		deserializeErr[NodeConfig](
			t,
			MinimalNodeConfigYaml,
			fmt.Sprintf("field %s must require >=0 integer", field),
			patch(fmt.Sprintf(`[{"op":"replace", "path":"/%s", "value":-1}]`, field)),
		)
	}
}

func TestConfigParsing(t *testing.T) {
	t.Parallel()

	deserializeErr[Config](t, "", "config must require a usable set of defaults")

	// verify it detects missing fields.
	for _, field := range []string{"path", "helperPod.yaml", "setup", "teardown"} {
		deserializeErr[Config](
			t,
			MinimalNodeConfigYaml,
			fmt.Sprintf("defaults field %s failed to be 'required'", field),
			patch(fmt.Sprintf(`[{"op": "remove", "path":"/%s"}]`, field)),
		)
	}

	obj := deserializeObj[Config](t, MinimalNodeConfigYaml, "miminal config must parse")
	require.Equal(t, obj.SetupTimeout, DEFAULT_CMD_TIMEOUT, "default value should be %s to assert undefined", DEFAULT_CMD_TIMEOUT)
	require.Equal(t, obj.TeardownTimeout, DEFAULT_CMD_TIMEOUT, "default value should be %s to assert undefined", DEFAULT_CMD_TIMEOUT)

}
