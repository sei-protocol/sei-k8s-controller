package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/util/jsonpath"
)

// ExtractOutputs evaluates each "<jsonpath>=<ENV_VAR>" spec against obj and
// returns the env-var assignments. JSONPath syntax is the standard
// k8s.io/client-go/util/jsonpath (the same used by `kubectl get -o jsonpath`).
//
// Missing fields are not errors — the resulting KV is omitted. This is load-
// bearing for sidecar-backed kinds whose status.outputs.<kind> is empty in MVP
// (see LLD: "No structured outputs from sidecar-backed kinds"); a runner step
// that targets such an output should not fail just because the field is
// missing — the Workflow author can decide whether downstream steps need it.
func ExtractOutputs(specs []string, obj map[string]any) ([]KV, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]KV, 0, len(specs))
	for _, spec := range specs {
		pathRaw, envVarRaw, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("output-jsonpath %q missing '=ENV_VAR' suffix", spec)
		}
		path := strings.TrimSpace(pathRaw)
		envVar := strings.TrimSpace(envVarRaw)
		if path == "" || envVar == "" {
			return nil, fmt.Errorf("output-jsonpath %q has empty path or env var", spec)
		}
		val, ok, err := evalJSONPath(path, obj)
		if err != nil {
			return nil, fmt.Errorf("evaluate %q: %w", path, err)
		}
		if !ok {
			continue
		}
		out = append(out, KV{Key: envVar, Value: val})
	}
	return out, nil
}

func evalJSONPath(path string, obj map[string]any) (string, bool, error) {
	jp := jsonpath.New("output").AllowMissingKeys(true)
	// kubectl's syntax accepts the leading dot (".status.phase"); the jsonpath
	// lib wants "{.status.phase}". Normalize.
	expr := path
	if !strings.HasPrefix(expr, "{") {
		expr = "{" + expr + "}"
	}
	if err := jp.Parse(expr); err != nil {
		return "", false, err
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, obj); err != nil {
		return "", false, err
	}
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}

// FileEnvSourcer loads KEY=VALUE pairs from a shell-style env file. Lines
// starting with '#' and blank lines are skipped. Quoted values are not
// unquoted — the runner's writes are unquoted by construction, so any quoting
// the operator authored is preserved verbatim.
type FileEnvSourcer struct{}

// Source reads KEY=VALUE pairs from path. Returns (nil, nil) when path
// doesn't exist — missing env files are not an error (first Workflow step
// has nothing to source).
func (FileEnvSourcer) Source(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-controlled CLI arg
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip a leading "export " so files written by other tools are accepted.
		line = strings.TrimPrefix(line, "export ")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		out[line[:idx]] = line[idx+1:]
	}
	return out, scanner.Err()
}

// FileEnvWriter appends KEY=value lines to an env file. Parent directories
// must already exist (Chaos Mesh Workflow mounts the emptyDir for us).
type FileEnvWriter struct{}

// Append appends KEY=value lines to path. The file is opened in append mode
// and flushed on close. Values are not quoted; values containing whitespace
// or shell metacharacters survive the runner's `source` because the runner
// reads with a line parser, not a shell. Operators wiring to a real
// `source` should keep values free of shell metacharacters — by convention,
// SeiNodeTask outputs (txHash, height, image) are.
func (FileEnvWriter) Append(path string, kv []KV) error {
	if len(kv) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // path is operator-controlled CLI arg
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for _, p := range kv {
		if _, err := fmt.Fprintf(f, "%s=%s\n", p.Key, p.Value); err != nil {
			return err
		}
	}
	return nil
}
