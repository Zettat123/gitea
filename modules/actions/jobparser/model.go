// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package jobparser

import (
	"bytes"
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

// SingleWorkflow is a workflow with single job and single matrix
type SingleWorkflow struct {
	Name           string            `yaml:"name,omitempty"`
	RawOn          yaml.Node         `yaml:"on,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"`
	RawJobs        yaml.Node         `yaml:"jobs,omitempty"`
	Defaults       Defaults          `yaml:"defaults,omitempty"`
	RawPermissions yaml.Node         `yaml:"permissions,omitempty"`
	RunName        string            `yaml:"run-name,omitempty"`
}

func (w *SingleWorkflow) Job() (string, *Job) {
	ids, jobs, _ := w.jobs()
	if len(ids) >= 1 {
		return ids[0], jobs[0]
	}
	return "", nil
}

func (w *SingleWorkflow) jobs() ([]string, []*Job, error) {
	ids, jobs, err := parseMappingNode[*Job](&w.RawJobs)
	if err != nil {
		return nil, nil, err
	}

	for _, job := range jobs {
		steps := make([]*Step, 0, len(job.Steps))
		for _, s := range job.Steps {
			if s != nil {
				steps = append(steps, s)
			}
		}
		job.Steps = steps
	}

	return ids, jobs, nil
}

func (w *SingleWorkflow) SetJob(id string, job *Job) error {
	m := map[string]*Job{
		id: job,
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(m); err != nil {
		return err
	}
	encoder.Close()

	node := yaml.Node{}
	if err := yaml.Unmarshal(buf.Bytes(), &node); err != nil {
		return err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("can not set job: %s", buf.String())
	}
	w.RawJobs = *node.Content[0]
	return nil
}

func (w *SingleWorkflow) Marshal() ([]byte, error) {
	return yaml.Marshal(w)
}

type Job struct {
	Name           string                    `yaml:"name,omitempty"`
	RawNeeds       yaml.Node                 `yaml:"needs,omitempty"`
	RawRunsOn      yaml.Node                 `yaml:"runs-on,omitempty"`
	Env            yaml.Node                 `yaml:"env,omitempty"`
	If             yaml.Node                 `yaml:"if,omitempty"`
	Steps          []*Step                   `yaml:"steps,omitempty"`
	TimeoutMinutes string                    `yaml:"timeout-minutes,omitempty"`
	Services       map[string]*ContainerSpec `yaml:"services,omitempty"`
	Strategy       Strategy                  `yaml:"strategy,omitempty"`
	RawContainer   yaml.Node                 `yaml:"container,omitempty"`
	Defaults       Defaults                  `yaml:"defaults,omitempty"`
	Outputs        map[string]string         `yaml:"outputs,omitempty"`
	Uses           string                    `yaml:"uses,omitempty"`
	With           map[string]any            `yaml:"with,omitempty"`
	RawSecrets     yaml.Node                 `yaml:"secrets,omitempty"`
	RawConcurrency *model.RawConcurrency     `yaml:"concurrency,omitempty"`
	RawPermissions yaml.Node                 `yaml:"permissions,omitempty"`
}

func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	return &Job{
		Name:           j.Name,
		RawNeeds:       j.RawNeeds,
		RawRunsOn:      j.RawRunsOn,
		Env:            j.Env,
		If:             j.If,
		Steps:          j.Steps,
		TimeoutMinutes: j.TimeoutMinutes,
		Services:       j.Services,
		Strategy:       j.Strategy,
		RawContainer:   j.RawContainer,
		Defaults:       j.Defaults,
		Outputs:        j.Outputs,
		Uses:           j.Uses,
		With:           j.With,
		RawSecrets:     j.RawSecrets,
		RawConcurrency: j.RawConcurrency,
		RawPermissions: j.RawPermissions,
	}
}

func (j *Job) Needs() []string {
	return (&model.Job{RawNeeds: j.RawNeeds}).Needs()
}

func (j *Job) EraseNeeds() *Job {
	j.RawNeeds = yaml.Node{}
	return j
}

func (j *Job) RunsOn() []string {
	return (&model.Job{RawRunsOn: j.RawRunsOn}).RunsOn()
}

type Step struct {
	ID               string            `yaml:"id,omitempty"`
	If               yaml.Node         `yaml:"if,omitempty"`
	Name             string            `yaml:"name,omitempty"`
	Uses             string            `yaml:"uses,omitempty"`
	Run              string            `yaml:"run,omitempty"`
	WorkingDirectory string            `yaml:"working-directory,omitempty"`
	Shell            string            `yaml:"shell,omitempty"`
	Env              yaml.Node         `yaml:"env,omitempty"`
	With             map[string]string `yaml:"with,omitempty"`
	ContinueOnError  bool              `yaml:"continue-on-error,omitempty"`
	TimeoutMinutes   string            `yaml:"timeout-minutes,omitempty"`
}

// String gets the name of step
func (s *Step) String() string {
	if s == nil {
		return ""
	}
	return (&model.Step{
		ID:   s.ID,
		Name: s.Name,
		Uses: s.Uses,
		Run:  s.Run,
	}).String()
}

type ContainerSpec struct {
	Image       string            `yaml:"image,omitempty"`
	Env         map[string]string `yaml:"env,omitempty"`
	Ports       []string          `yaml:"ports,omitempty"`
	Volumes     []string          `yaml:"volumes,omitempty"`
	Options     string            `yaml:"options,omitempty"`
	Credentials map[string]string `yaml:"credentials,omitempty"`
	Cmd         []string          `yaml:"cmd,omitempty"`
}

type Strategy struct {
	FailFastString    string    `yaml:"fail-fast,omitempty"`
	MaxParallelString string    `yaml:"max-parallel,omitempty"`
	RawMatrix         yaml.Node `yaml:"matrix,omitempty"`
}

type Defaults struct {
	Run RunDefaults `yaml:"run,omitempty"`
}

type RunDefaults struct {
	Shell            string `yaml:"shell,omitempty"`
	WorkingDirectory string `yaml:"working-directory,omitempty"`
}

type WorkflowDispatchInput struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	Default     string   `yaml:"default"`
	Type        string   `yaml:"type"`
	Options     []string `yaml:"options"`
}

type Event struct {
	Name      string
	acts      map[string][]string
	schedules []map[string]string
	inputs    []WorkflowDispatchInput
}

func (evt *Event) IsSchedule() bool {
	return evt.schedules != nil
}

func (evt *Event) Acts() map[string][]string {
	return evt.acts
}

func (evt *Event) Schedules() []map[string]string {
	return evt.schedules
}

func (evt *Event) Inputs() []WorkflowDispatchInput {
	return evt.inputs
}

func ReadWorkflowRawConcurrency(content []byte) (*model.RawConcurrency, error) {
	w := new(model.Workflow)
	err := yaml.NewDecoder(bytes.NewReader(content)).Decode(w)
	return w.RawConcurrency, err
}

func EvaluateConcurrency(rc *model.RawConcurrency, jobID string, job *Job, gitCtx map[string]any, results map[string]*JobResult, vars map[string]string, inputs map[string]any) (string, bool, error) {
	actJob := &model.Job{}
	if job != nil {
		actJob.Strategy = &model.Strategy{
			FailFastString:    job.Strategy.FailFastString,
			MaxParallelString: job.Strategy.MaxParallelString,
			RawMatrix:         job.Strategy.RawMatrix,
		}
		actJob.Strategy.FailFast = actJob.Strategy.GetFailFast()
		actJob.Strategy.MaxParallel = actJob.Strategy.GetMaxParallel()
	}

	matrix := make(map[string]any)
	matrixes, err := actJob.GetMatrixes()
	if err != nil {
		return "", false, err
	}
	if len(matrixes) > 0 {
		matrix = matrixes[0]
	}

	evaluator := NewExpressionEvaluator(NewInterpeter(jobID, actJob, matrix, toGitContext(gitCtx), results, vars, inputs))
	var node yaml.Node
	if err := node.Encode(rc); err != nil {
		return "", false, fmt.Errorf("failed to encode concurrency: %w", err)
	}
	if err := evaluator.EvaluateYamlNode(&node); err != nil {
		return "", false, fmt.Errorf("failed to evaluate concurrency: %w", err)
	}
	var evaluated model.RawConcurrency
	if err := node.Decode(&evaluated); err != nil {
		return "", false, fmt.Errorf("failed to unmarshal evaluated concurrency: %w", err)
	}
	if evaluated.RawExpression != "" {
		return evaluated.RawExpression, false, nil
	}
	return evaluated.Group, evaluated.CancelInProgress == "true", nil
}

// EvaluateWorkflowCallInputs evaluates reusable workflow call inputs and returns the final input map.
func EvaluateWorkflowCallInputs(workflow *model.Workflow, jobID string, job *Job, gitCtx map[string]any, results map[string]*JobResult, vars map[string]string, inputs map[string]any) (map[string]any, error) {
	cfg := workflow.WorkflowCallConfig()
	if len(cfg.Inputs) == 0 {
		return map[string]any{}, nil
	}

	provided := make(container.Set[string], len(job.With))

	inputsWithDefaults := make(map[string]any)
	for name, input := range cfg.Inputs {
		if input.Type == "" {
			return nil, fmt.Errorf("workflow_call input %q is missing required field \"type\"", name)
		}

		if input.Default == "" {
			continue
		}

		switch input.Type {
		case "string":
			inputsWithDefaults[name] = input.Default
		case "boolean":
			v, err := strconv.ParseBool(input.Default)
			if err != nil {
				return nil, fmt.Errorf("parse workflow_call input %q default: %w", name, err)
			}
			inputsWithDefaults[name] = v
		case "number":
			v, err := util.ToFloat64(input.Default)
			if err != nil {
				return nil, fmt.Errorf("parse workflow_call input %q default: %w", name, err)
			}
			inputsWithDefaults[name] = v
		default:
			return nil, fmt.Errorf("unknown workflow_call input type %q", input.Type)
		}
	}

	actJob := &model.Job{
		Strategy: &model.Strategy{
			FailFastString:    job.Strategy.FailFastString,
			MaxParallelString: job.Strategy.MaxParallelString,
			RawMatrix:         job.Strategy.RawMatrix,
		},
	}
	actJob.Strategy.FailFast = actJob.Strategy.GetFailFast()
	actJob.Strategy.MaxParallel = actJob.Strategy.GetMaxParallel()
	matrix := make(map[string]any)
	matrixes, err := actJob.GetMatrixes()
	if err != nil {
		return nil, err
	}
	if len(matrixes) > 0 {
		matrix = matrixes[0]
	}

	evaluator := NewExpressionEvaluator(NewInterpeter(jobID, actJob, matrix, toGitContext(gitCtx), results, vars, inputs))
	for k, v := range job.With {
		wcInput, ok := cfg.Inputs[k]
		if !ok {
			continue
		}
		provided.Add(k)

		var out any
		switch val := v.(type) {
		case string:
			var node yaml.Node
			if err := node.Encode(val); err != nil {
				return nil, fmt.Errorf("encode workflow_call input %q: %w", k, err)
			}
			if err := evaluator.EvaluateYamlNode(&node); err != nil {
				return nil, fmt.Errorf("evaluate workflow_call input %q: %w", k, err)
			}
			if err := node.Decode(&out); err != nil {
				return nil, fmt.Errorf("decode workflow_call input %q: %w", k, err)
			}
		default:
			out = v
		}

		switch wcInput.Type {
		case "string":
			inputsWithDefaults[k] = out
		case "boolean":
			switch out.(type) {
			case bool:
				inputsWithDefaults[k] = out
			default:
				return nil, fmt.Errorf("workflow_call input %q expects boolean", k)
			}
		case "number":
			f, err := util.ToFloat64(out)
			if err != nil {
				return nil, fmt.Errorf("workflow_call input %q expects number: %w", k, err)
			}
			inputsWithDefaults[k] = f
		default:
			return nil, fmt.Errorf("unknown workflow_call input type %q for input %q", wcInput.Type, k)
		}
	}

	for name, input := range cfg.Inputs {
		if input.Required {
			if provided.Contains(name) {
				continue
			}
			if input.Default == "" {
				return nil, fmt.Errorf("workflow_call input %q is required", name)
			}
		}
	}

	return inputsWithDefaults, nil
}

var workflowCallSecretExpr = regexp.MustCompile(`^\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)

// EvaluateWorkflowCallSecrets evaluates reusable workflow call secrets mapping.
// It only accepts values in the form "${{ secrets.NAME }}".
func EvaluateWorkflowCallSecrets(workflow *model.Workflow, job *Job) (map[string]string, error) {
	cfg := workflow.WorkflowCallConfig()
	if len(cfg.Secrets) == 0 {
		return map[string]string{}, nil
	}

	allowed := cfg.Secrets
	secrets := make(map[string]string)

	if job == nil || job.RawSecrets.IsZero() {
		for name, secret := range allowed {
			if secret.Required {
				return nil, fmt.Errorf("workflow_call secret %q is required", name)
			}
		}
		return secrets, nil
	}

	var raw map[string]string
	if err := job.RawSecrets.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode secrets: %w", err)
	}

	for name, val := range raw {
		secretName, err := parseWorkflowCallSecretValue(val)
		if err != nil {
			return nil, fmt.Errorf("workflow_call secret %q: %w", name, err)
		}
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("workflow_call secret %q is not declared", name)
		}
		secrets[name] = secretName
	}

	for name, secret := range allowed {
		if secret.Required {
			if _, ok := secrets[name]; !ok {
				return nil, fmt.Errorf("workflow_call secret %q is required", name)
			}
		}
	}

	return secrets, nil
}

func parseWorkflowCallSecretValue(val string) (string, error) {
	matches := workflowCallSecretExpr.FindStringSubmatch(val)
	if len(matches) != 2 {
		return "", errors.New("secret value must be in the form \"${{ secrets.NAME }}\"")
	}
	return matches[1], nil
}

// EvaluateWorkflowCallOutputs evaluates reusable workflow call outputs with expression support.
// It accepts any valid expression and evaluates it using the provided contexts.
func EvaluateWorkflowCallOutputs(workflow *model.Workflow, gitCtx map[string]any, vars map[string]string, inputs map[string]any, jobOutputs map[string]map[string]string) (map[string]string, error) {
	cfg := workflow.WorkflowCallConfig()
	if len(cfg.Outputs) == 0 {
		return map[string]string{}, nil
	}

	jobsCtx := make(map[string]*model.WorkflowCallResult, len(jobOutputs))
	for jobID, outputs := range jobOutputs {
		jobsCtx[jobID] = &model.WorkflowCallResult{Outputs: outputs}
	}

	env := &exprparser.EvaluationEnvironment{
		Github: toGitContext(gitCtx),
		Vars:   vars,
		Inputs: inputs,
		Jobs:   &jobsCtx,
	}
	interpreter := exprparser.NewInterpeter(env, exprparser.Config{})

	result := make(map[string]string, len(cfg.Outputs))
	for name, outputItem := range cfg.Outputs {
		value, err := evaluateWorkflowCallOutputValue(interpreter, outputItem.Value)
		if err != nil {
			return nil, fmt.Errorf("workflow_call output %q: %w", name, err)
		}
		result[name] = value
	}

	return result, nil
}

func evaluateWorkflowCallOutputValue(interpreter exprparser.Interpreter, value string) (string, error) {
	if !strings.Contains(value, "${{") || !strings.Contains(value, "}}") {
		return value, nil
	}
	expr, _ := rewriteSubExpression(value, true)
	evaluated, err := interpreter.Evaluate(expr, exprparser.DefaultStatusCheckNone)
	if err != nil {
		return "", err
	}
	str, ok := evaluated.(string)
	if !ok {
		return "", fmt.Errorf("expression %q did not evaluate to a string", expr)
	}
	return str, nil
}

func toGitContext(input map[string]any) *model.GithubContext {
	gitContext := &model.GithubContext{
		EventPath:        asString(input["event_path"]),
		Workflow:         asString(input["workflow"]),
		RunID:            asString(input["run_id"]),
		RunNumber:        asString(input["run_number"]),
		Actor:            asString(input["actor"]),
		Repository:       asString(input["repository"]),
		EventName:        asString(input["event_name"]),
		Sha:              asString(input["sha"]),
		Ref:              asString(input["ref"]),
		RefName:          asString(input["ref_name"]),
		RefType:          asString(input["ref_type"]),
		HeadRef:          asString(input["head_ref"]),
		BaseRef:          asString(input["base_ref"]),
		Token:            asString(input["token"]),
		Workspace:        asString(input["workspace"]),
		Action:           asString(input["action"]),
		ActionPath:       asString(input["action_path"]),
		ActionRef:        asString(input["action_ref"]),
		ActionRepository: asString(input["action_repository"]),
		Job:              asString(input["job"]),
		RepositoryOwner:  asString(input["repository_owner"]),
		RetentionDays:    asString(input["retention_days"]),
	}

	event, ok := input["event"].(map[string]any)
	if ok {
		gitContext.Event = event
	}

	return gitContext
}

func ParseRawOn(rawOn *yaml.Node) ([]*Event, error) {
	switch rawOn.Kind {
	case yaml.ScalarNode:
		var val string
		err := rawOn.Decode(&val)
		if err != nil {
			return nil, err
		}
		return []*Event{
			{Name: val},
		}, nil
	case yaml.SequenceNode:
		var val []any
		err := rawOn.Decode(&val)
		if err != nil {
			return nil, err
		}
		res := make([]*Event, 0, len(val))
		for _, v := range val {
			switch t := v.(type) {
			case string:
				res = append(res, &Event{Name: t})
			default:
				return nil, fmt.Errorf("invalid type %T", t)
			}
		}
		return res, nil
	case yaml.MappingNode:
		events, triggers, err := parseMappingNode[yaml.Node](rawOn)
		if err != nil {
			return nil, err
		}
		res := make([]*Event, 0, len(events))
		for i, k := range events {
			v := triggers[i]
			switch v.Kind {
			case yaml.ScalarNode:
				res = append(res, &Event{
					Name: k,
				})
			case yaml.SequenceNode:
				var t []any
				err := v.Decode(&t)
				if err != nil {
					return nil, err
				}
				schedules := make([]map[string]string, len(t))
				if k == "schedule" {
					for i, tt := range t {
						vv, ok := tt.(map[string]any)
						if !ok {
							return nil, fmt.Errorf("unknown on type(schedule): %#v", v)
						}
						schedules[i] = make(map[string]string, len(vv))
						for k, vvv := range vv {
							var ok bool
							if schedules[i][k], ok = vvv.(string); !ok {
								return nil, fmt.Errorf("unknown on type(schedule): %#v", v)
							}
						}
					}
				}

				if len(schedules) == 0 {
					schedules = nil
				}
				res = append(res, &Event{
					Name:      k,
					schedules: schedules,
				})
			case yaml.MappingNode:
				acts := make(map[string][]string, len(v.Content)/2)
				var inputs []WorkflowDispatchInput
				expectedKey := true
				var act string
				for _, content := range v.Content {
					if expectedKey {
						if content.Kind != yaml.ScalarNode {
							return nil, fmt.Errorf("key type not string: %#v", content)
						}
						act = ""
						err := content.Decode(&act)
						if err != nil {
							return nil, err
						}
					} else {
						switch content.Kind {
						case yaml.SequenceNode:
							var t []string
							err := content.Decode(&t)
							if err != nil {
								return nil, err
							}
							acts[act] = t
						case yaml.ScalarNode:
							var t string
							err := content.Decode(&t)
							if err != nil {
								return nil, err
							}
							acts[act] = []string{t}
						case yaml.MappingNode:
							if k != "workflow_dispatch" && k != "workflow_call" {
								return nil, fmt.Errorf("map should only for workflow_dispatch or workflow_call but %s: %#v", act, content)
							}
							if act != "inputs" {
								// workflow_call may also contain "secrets" and "outputs".
								// They are parsed elsewhere, here we only need to avoid treating them as invalid workflows.
								break
							}

							var key string
							for i, vv := range content.Content {
								if i%2 == 0 {
									if vv.Kind != yaml.ScalarNode {
										return nil, fmt.Errorf("key type not string: %#v", vv)
									}
									key = ""
									if err := vv.Decode(&key); err != nil {
										return nil, err
									}
								} else {
									if vv.Kind != yaml.MappingNode {
										return nil, fmt.Errorf("key type not map(%s): %#v", key, vv)
									}

									input := WorkflowDispatchInput{}
									if err := vv.Decode(&input); err != nil {
										return nil, err
									}
									input.Name = key
									inputs = append(inputs, input)
								}
							}
						default:
							return nil, fmt.Errorf("unknown on type: %#v", content)
						}
					}
					expectedKey = !expectedKey
				}
				if len(inputs) == 0 {
					inputs = nil
				}
				if len(acts) == 0 {
					acts = nil
				}
				res = append(res, &Event{
					Name:   k,
					acts:   acts,
					inputs: inputs,
				})
			default:
				return nil, fmt.Errorf("unknown on type: %v", v.Kind)
			}
		}
		return res, nil
	default:
		return nil, fmt.Errorf("unknown on type: %v", rawOn.Kind)
	}
}

// parseMappingNode parse a mapping node and preserve order.
func parseMappingNode[T any](node *yaml.Node) ([]string, []T, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nil, errors.New("input node is not a mapping node")
	}

	var scalars []string
	var datas []T
	expectKey := true
	for _, item := range node.Content {
		if expectKey {
			if item.Kind != yaml.ScalarNode {
				return nil, nil, fmt.Errorf("not a valid scalar node: %v", item.Value)
			}
			scalars = append(scalars, item.Value)
			expectKey = false
		} else {
			var val T
			if err := item.Decode(&val); err != nil {
				return nil, nil, err
			}
			datas = append(datas, val)
			expectKey = true
		}
	}

	if len(scalars) != len(datas) {
		return nil, nil, fmt.Errorf("invalid definition of on: %v", node.Value)
	}

	return scalars, datas, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	} else if s, ok := v.(string); ok {
		return s
	}
	return ""
}
