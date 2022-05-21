package main

import (
	"fmt"
	"io/ioutil"
	"os"

	encoding_json "encoding/json"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

func loadFile(filepath string) (string, error) {
	f, err := os.Open(filepath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	helperPodYaml, err := ioutil.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(helperPodYaml), nil
}

// strictJSONUnmarshal is a helper to force strict unmarshalling of JSON, collapsing the results into a multierror
func strictJSONUnmarshal[P interface{ *T }, T any](b []byte, result P) error {
	serr, err := json.UnmarshalStrict(b, result)
	return multierror.Append(err, serr...).ErrorOrNil()
}

// strictJSONUnmarshal is a helper to force strict YAML unmarshalling of structs supporting JSON, collapsing the results into a multierror
func strictYAMLUnmarshal[P interface{ *T }, T any](b []byte, result P) error {
	jsonData, err := yaml.YAMLToJSONStrict([]byte(b))
	// correct a yaml -> json difference; yaml.Unmarshal("") yields "", which json doesn't
	// view as an object; effectively it bypasses the actual parsing.
	if len(jsonData) == 0 {
		jsonData = []byte("{}")
	}
	if err != nil {
		return err
	}
	return errors.Wrap(strictJSONUnmarshal(jsonData, result), "while converting YAML to json")
}

func jsonStringer[T any](obj T) string {
	b, err := encoding_json.MarshalIndent(obj, "", " ")
	if err != nil {
		return fmt.Sprintf("failed to stringify due to %s", err)
	}
	return string(b)
}
