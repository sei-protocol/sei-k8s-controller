package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// configValuesOverlay builds an independent merge patch. In
// particular, these values must never pass through ConfigIntent.Overrides.
func configValuesOverlay(values []seiv1alpha1.ConfigValue) (*task.ConfigPatchTask, error) {
	patch := &task.ConfigPatchTask{Files: make(map[string]map[string]any)}
	origins := make(map[string]*overlayOrigin)
	for _, entry := range values {
		// Mirror ConfigValue.FileName admission validation for callers bypassing the API.
		if !configValueFileNamePattern.MatchString(entry.FileName) {
			return nil, fmt.Errorf("configValues %s:%s: fileName must match %s", entry.FileName, entry.Key, configValueFileNamePattern)
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(entry.Value.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("configValues %s:%s: %w", entry.FileName, entry.Key, err)
		}
		if containsNull(value) {
			return nil, fmt.Errorf("configValues %s:%s: null values are not supported, including inside objects or arrays; supply a TOML value", entry.FileName, entry.Key)
		}
		if err := validateOverlayNumbers(value); err != nil {
			return nil, fmt.Errorf("configValues %s:%s: %w", entry.FileName, entry.Key, err)
		}
		path := strings.Split(entry.Key, ".")
		for _, key := range slices.Backward(path) {
			value = map[string]any{key: value}
		}
		if patch.Files[entry.FileName] == nil {
			patch.Files[entry.FileName] = make(map[string]any)
			origins[entry.FileName] = &overlayOrigin{children: make(map[string]*overlayOrigin)}
		}
		if previous := insertOverlay(patch.Files[entry.FileName], value.(map[string]any), origins[entry.FileName], entry.Key); previous != "" {
			return nil, fmt.Errorf("configValues %s: overlapping keys %q and %q; use non-overlapping dotted paths", entry.FileName, previous, entry.Key)
		}
	}
	return patch, nil
}

// withConfigValues captures the desired config and applies the overlay after
// base regeneration and any state-sync/genesis peer writes, before validation.
// Bootstrap plans have two validation stages and need the overlay in both.
func withConfigValues(plan *seiv1alpha1.TaskPlan, node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	hash, err := configValuesHash(node.Spec.ConfigValues)
	if err != nil {
		return nil, err
	}
	plan.ConfigValuesHash = hash
	if len(node.Spec.ConfigValues) == 0 {
		return plan, nil
	}
	patch, err := configValuesOverlay(node.Spec.ConfigValues)
	if err != nil {
		return nil, err
	}
	tasks := make([]seiv1alpha1.PlannedTask, 0, 2*len(plan.Tasks))
	for _, planned := range plan.Tasks {
		if planned.Type == TaskConfigValidate {
			overlay, err := buildPlannedTask(plan.ID, TaskConfigPatch, len(tasks), patch)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, overlay)
		}
		planned.ID = task.DeterministicTaskID(plan.ID, planned.Type, len(tasks))
		tasks = append(tasks, planned)
	}
	if len(tasks) == len(plan.Tasks) {
		return nil, fmt.Errorf("configValues: cannot splice overlay: plan has no config-validate task")
	}
	plan.Tasks = tasks
	return plan, nil
}

// containsNull rejects merge-patch deletion semantics and unencodable TOML nils.
func containsNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, containsNull)
	}
	return false
}

var configValueFileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.toml$`)

// overlayOrigin tracks the entry that supplied each node, including table members.
type overlayOrigin struct {
	key      string
	children map[string]*overlayOrigin
}

// insertOverlay only creates nodes or merges map into map. Any other collision
// rejects the whole patch; partial insertion is never returned to the caller.
func insertOverlay(dst, src map[string]any, origin *overlayOrigin, key string) string {
	for name, value := range src {
		previous, exists := dst[name]
		incoming, isMap := value.(map[string]any)
		current, wasMap := previous.(map[string]any)
		if exists && (!isMap || !wasMap) {
			return origin.children[name].key
		}
		if !exists {
			origin.children[name] = &overlayOrigin{key: key, children: make(map[string]*overlayOrigin)}
		}
		if isMap {
			if !exists {
				current = make(map[string]any)
			}
			if conflict := insertOverlay(current, incoming, origin.children[name], key); conflict != "" {
				return conflict
			}
			dst[name] = current
		} else {
			dst[name] = value
		}
	}
	return ""
}

// Match the TOML encoder's int64-first, float64-fallback numeric conversion.
func validateOverlayNumbers(value any) error {
	switch value := value.(type) {
	case json.Number:
		if _, err := value.Int64(); err != nil {
			if _, err := value.Float64(); err != nil {
				return fmt.Errorf("number %q cannot be represented as int64 or float64: %w", value, err)
			}
		}
	case map[string]any:
		for _, child := range value {
			if err := validateOverlayNumbers(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateOverlayNumbers(child); err != nil {
				return err
			}
		}
	}
	return nil
}

// configValuesHash hashes sorted JSON tuples. Decode with UseNumber to preserve
// large integer precision; Marshal sorts object keys and removes whitespace.
// Sorting the complete tuples makes list order and Go map iteration irrelevant.
func configValuesHash(values []seiv1alpha1.ConfigValue) (string, error) {
	tuples := make([]string, 0, len(values))
	for _, entry := range values {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(entry.Value.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return "", fmt.Errorf("configValues %s:%s: %w", entry.FileName, entry.Key, err)
		}
		tuple, err := json.Marshal([]any{entry.FileName, entry.Key, value})
		if err != nil {
			return "", fmt.Errorf("hashing configValues: %w", err)
		}
		tuples = append(tuples, string(tuple))
	}
	slices.Sort(tuples)
	encoded, err := json.Marshal(tuples)
	if err != nil {
		return "", fmt.Errorf("hashing configValues tuples: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
