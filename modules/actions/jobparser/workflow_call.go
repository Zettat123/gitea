// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package jobparser

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/util"

	"github.com/nektos/act/pkg/exprparser"
	"github.com/nektos/act/pkg/model"
	"go.yaml.in/yaml/v4"
)

// InputType enumerates the allowed types for a workflow_call input.
type InputType string

const (
	InputTypeString  InputType = "string"
	InputTypeBoolean InputType = "boolean"
	InputTypeNumber  InputType = "number"
)

// InputSpec describes a single workflow_call input declaration.
type InputSpec struct {
	Description string    `yaml:"description"`
	Required    bool      `yaml:"required"`
	Default     yaml.Node `yaml:"default"`
	Type        InputType `yaml:"type"`
}

// SecretSpec describes a single workflow_call secret declaration.
type SecretSpec struct {
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// OutputSpec describes a single workflow_call output declaration.
type OutputSpec struct {
	Description string `yaml:"description"`
	Value       string `yaml:"value"`
}

// WorkflowCallSpec is the parsed "on.workflow_call" schema of a called workflow.
type WorkflowCallSpec struct {
	Inputs  map[string]InputSpec
	Secrets map[string]SecretSpec
	Outputs map[string]OutputSpec
}

// JobOutputs is the per-job-id outputs map used for evaluating workflow_call outputs.
type JobOutputs map[string]map[string]string

// ParseWorkflowCallSpec extracts on.workflow_call.{inputs,secrets,outputs} from a workflow YAML.
// Returns an error if the workflow does not declare on.workflow_call at all
// (i.e. it is not callable as a reusable workflow).
func ParseWorkflowCallSpec(content []byte) (*WorkflowCallSpec, error) {
	var doc struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("parse workflow yaml: %w", err)
	}

	wcNode, ok := findWorkflowCallNode(&doc.On)
	if !ok {
		return nil, errors.New("workflow does not declare on.workflow_call")
	}

	spec := &WorkflowCallSpec{
		Inputs:  map[string]InputSpec{},
		Secrets: map[string]SecretSpec{},
		Outputs: map[string]OutputSpec{},
	}

	if wcNode == nil || wcNode.Kind != yaml.MappingNode {
		return spec, nil
	}

	for i := 0; i+1 < len(wcNode.Content); i += 2 {
		key := wcNode.Content[i]
		val := wcNode.Content[i+1]
		switch key.Value {
		case "inputs":
			if err := decodeWorkflowCallMapping(val, spec.Inputs); err != nil {
				return nil, fmt.Errorf("parse workflow_call.inputs: %w", err)
			}
		case "secrets":
			if err := decodeWorkflowCallMapping(val, spec.Secrets); err != nil {
				return nil, fmt.Errorf("parse workflow_call.secrets: %w", err)
			}
		case "outputs":
			if err := decodeWorkflowCallMapping(val, spec.Outputs); err != nil {
				return nil, fmt.Errorf("parse workflow_call.outputs: %w", err)
			}
		}
	}

	for name, in := range spec.Inputs {
		if in.Type == "" {
			return nil, fmt.Errorf("workflow_call input %q is missing required field \"type\"", name)
		}
		switch in.Type {
		case InputTypeString, InputTypeBoolean, InputTypeNumber:
		default:
			return nil, fmt.Errorf("workflow_call input %q has unsupported type %q", name, in.Type)
		}
	}

	return spec, nil
}

// findWorkflowCallNode walks the "on:" node and returns the value mapping (or nil) for "workflow_call".
// "ok" is true when the workflow declares workflow_call (even with an empty body).
func findWorkflowCallNode(on *yaml.Node) (val *yaml.Node, ok bool) {
	if on == nil || on.Kind == 0 {
		return nil, false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return nil, on.Value == "workflow_call"
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if item.Kind == yaml.ScalarNode && item.Value == "workflow_call" {
				return nil, true
			}
		}
		return nil, false
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			k := on.Content[i]
			v := on.Content[i+1]
			if k.Value != "workflow_call" {
				continue
			}
			if v.Kind == yaml.MappingNode {
				return v, true
			}
			return nil, true
		}
	}
	return nil, false
}

func decodeWorkflowCallMapping[T any](node *yaml.Node, dst map[string]T) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		name := node.Content[i].Value
		var v T
		if err := node.Content[i+1].Decode(&v); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
		dst[name] = v
	}
	return nil
}

// EvaluateWorkflowCallInput resolves a reusable workflow caller's `with:`
// mapping into the inputs map exposed to the called workflow, validated against the
// called workflow's `on.workflow_call.inputs` schema.
//
// `reusableWorkflowContent` is the raw bytes of the called workflow file (typically cached
// on the caller row at expansion time). The schema is parsed inside; the evaluator
// is built internally so callers don't have to construct an ExpressionEvaluator.
//
// `job` is the caller's parsed Job (its `With` field provides the raw expressions).
// `gitCtx` / `results` / `vars` / `inputs` populate the evaluator's contexts the same
// way EvaluateJobIfExpression does.
func EvaluateWorkflowCallInput(
	jobID string,
	job *Job,
	reusableWorkflowContent []byte,
	gitCtx map[string]any,
	results map[string]*JobResult,
	vars map[string]string,
	inputs map[string]any,
) (map[string]any, error) {
	if len(reusableWorkflowContent) == 0 {
		return nil, fmt.Errorf("caller %q has no cached source content", jobID)
	}
	spec, err := ParseWorkflowCallSpec(reusableWorkflowContent)
	if err != nil {
		return nil, err
	}
	actJob := &model.Job{}
	if job != nil {
		actJob.Strategy = &model.Strategy{
			FailFastString:    job.Strategy.FailFastString,
			MaxParallelString: job.Strategy.MaxParallelString,
			RawMatrix:         job.Strategy.RawMatrix,
		}
	}
	evaluator := NewExpressionEvaluator(NewInterpeter(jobID, actJob, nil, toGitContext(gitCtx), results, vars, inputs))

	resolved := make(map[string]any, len(spec.Inputs))

	// fill defaults first
	for name, in := range spec.Inputs {
		if in.Default.IsZero() {
			continue
		}
		v, err := decodeWorkflowCallInputDefault(name, in)
		if err != nil {
			return nil, err
		}
		resolved[name] = v
	}

	provided := make(container.Set[string], len(job.With))
	for k, raw := range job.With {
		inputSpec, ok := spec.Inputs[k]
		if !ok {
			// ignore unknown "with:" keys
			continue
		}
		provided.Add(k)

		var evaluated any
		switch v := raw.(type) {
		case string:
			node := yaml.Node{}
			if err := node.Encode(v); err != nil {
				return nil, fmt.Errorf("encode workflow_call input %q: %w", k, err)
			}
			if err := evaluator.EvaluateYamlNode(&node); err != nil {
				return nil, fmt.Errorf("evaluate workflow_call input %q: %w", k, err)
			}
			if err := node.Decode(&evaluated); err != nil {
				return nil, fmt.Errorf("decode workflow_call input %q: %w", k, err)
			}
		default:
			evaluated = v
		}

		converted, err := coerceWorkflowCallInput(k, inputSpec.Type, evaluated)
		if err != nil {
			return nil, err
		}
		resolved[k] = converted
	}

	for name, in := range spec.Inputs {
		if !in.Required {
			continue
		}
		if provided.Contains(name) {
			continue
		}
		if _, ok := resolved[name]; ok {
			continue
		}
		return nil, fmt.Errorf("workflow_call input %q is required", name)
	}

	return resolved, nil
}

func decodeWorkflowCallInputDefault(name string, in InputSpec) (any, error) {
	var raw string
	if err := in.Default.Decode(&raw); err != nil {
		// non-scalar default — decode into "any"
		var anyVal any
		if err := in.Default.Decode(&anyVal); err != nil {
			return nil, fmt.Errorf("decode workflow_call input %q default: %w", name, err)
		}
		return coerceWorkflowCallInput(name, in.Type, anyVal)
	}
	return coerceWorkflowCallInput(name, in.Type, raw)
}

func coerceWorkflowCallInput(name string, typ InputType, v any) (any, error) {
	switch typ {
	case InputTypeString:
		return toString(v), nil
	case InputTypeBoolean:
		switch b := v.(type) {
		case bool:
			return b, nil
		case string:
			parsed, err := strconv.ParseBool(b)
			if err != nil {
				return false, fmt.Errorf("workflow_call input %q expects boolean, got %q", name, b)
			}
			return parsed, nil
		default:
			return false, fmt.Errorf("workflow_call input %q expects boolean", name)
		}
	case InputTypeNumber:
		return util.ToFloat64(v)
	default:
		return nil, fmt.Errorf("workflow_call input %q has unsupported type %q", name, typ)
	}
}

var workflowCallSecretValueRegexp = regexp.MustCompile(`^\s*\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}\s*$`)

// EvaluateWorkflowCallSecrets validates the caller's "secrets:" section against the called workflow's
// workflow_call secrets schema. It returns:
//   - inherit:  true when the caller declared "secrets: inherit"
//   - mapping:  {alias: source_name} (names only; values are resolved later at task pick time)
//
// rawSecrets is the {alias: raw-value} map decoded from the caller's "secrets:" section.
// The "inherit" form is signalled by passing `inheritScalar=true`.
func EvaluateWorkflowCallSecrets(spec *WorkflowCallSpec, inheritScalar bool, rawSecrets map[string]string) (inherit bool, mapping map[string]string, err error) {
	if inheritScalar {
		// inherit covers everything; required check is satisfied
		return true, nil, nil
	}

	mapping = make(map[string]string, len(rawSecrets))
	for alias, val := range rawSecrets {
		if _, ok := spec.Secrets[alias]; !ok {
			return false, nil, fmt.Errorf("workflow_call secret %q is not declared by the called workflow", alias)
		}
		matches := workflowCallSecretValueRegexp.FindStringSubmatch(val)
		if len(matches) != 2 {
			return false, nil, fmt.Errorf("workflow_call secret %q value must be of the form ${{ secrets.NAME }}", alias)
		}
		mapping[alias] = matches[1]
	}

	for name, sec := range spec.Secrets {
		if !sec.Required {
			continue
		}
		if _, ok := mapping[name]; !ok {
			return false, nil, fmt.Errorf("workflow_call secret %q is required", name)
		}
	}

	return false, mapping, nil
}

// EvaluateWorkflowCallOutputs evaluates a called workflow's "on.workflow_call.outputs.<name>.value"
// expressions against the provided contexts.
func EvaluateWorkflowCallOutputs(spec *WorkflowCallSpec, gitCtx *model.GithubContext, vars map[string]string, inputs map[string]any, jobOutputs JobOutputs) (map[string]string, error) {
	if spec == nil || len(spec.Outputs) == 0 {
		return map[string]string{}, nil
	}

	jobsCtx := make(map[string]*model.WorkflowCallResult, len(jobOutputs))
	for jobID, outputs := range jobOutputs {
		jobsCtx[jobID] = &model.WorkflowCallResult{Outputs: outputs}
	}

	env := &exprparser.EvaluationEnvironment{
		Github: gitCtx,
		Vars:   vars,
		Inputs: inputs,
		Jobs:   &jobsCtx,
	}
	interpreter := exprparser.NewInterpeter(env, exprparser.Config{})

	out := make(map[string]string, len(spec.Outputs))
	for name, o := range spec.Outputs {
		v, err := evaluateWorkflowCallOutputValue(interpreter, o.Value)
		if err != nil {
			return nil, fmt.Errorf("workflow_call output %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func evaluateWorkflowCallOutputValue(interpreter exprparser.Interpreter, value string) (string, error) {
	if !strings.Contains(value, "${{") || !strings.Contains(value, "}}") {
		return value, nil
	}
	expr, err := rewriteSubExpression(value, true)
	if err != nil {
		return "", err
	}
	evaluated, err := interpreter.Evaluate(expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return "", err
	}
	return toString(evaluated), nil
}

func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", s)
	}
}
