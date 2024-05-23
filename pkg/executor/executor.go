/*
Copyright 2024 The KubeSphere Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package executor

import (
	"context"
	"fmt"
	"path"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kkcorev1 "github.com/kubesphere/kubekey/v4/pkg/apis/core/v1"
	kubekeyv1 "github.com/kubesphere/kubekey/v4/pkg/apis/kubekey/v1"
	kubekeyv1alpha1 "github.com/kubesphere/kubekey/v4/pkg/apis/kubekey/v1alpha1"
	"github.com/kubesphere/kubekey/v4/pkg/converter"
	"github.com/kubesphere/kubekey/v4/pkg/converter/tmpl"
	"github.com/kubesphere/kubekey/v4/pkg/modules"
	"github.com/kubesphere/kubekey/v4/pkg/project"
	"github.com/kubesphere/kubekey/v4/pkg/variable"
)

// TaskExecutor all task in pipeline
type TaskExecutor interface {
	Exec(ctx context.Context) error
}

func NewTaskExecutor(client ctrlclient.Client, pipeline *kubekeyv1.Pipeline) TaskExecutor {
	// get variable
	v, err := variable.GetVariable(client, *pipeline)
	if err != nil {
		klog.V(4).ErrorS(nil, "convert playbook error", "pipeline", ctrlclient.ObjectKeyFromObject(pipeline))
		return nil
	}

	return &executor{
		client:   client,
		pipeline: pipeline,
		variable: v,
	}
}

type executor struct {
	client ctrlclient.Client

	pipeline *kubekeyv1.Pipeline
	variable variable.Variable
}

type execBlockOptions struct {
	// playbook level config
	hosts []string // which hosts will run playbook
	// blocks level config
	blocks []kkcorev1.Block
	role   string   // role name of blocks
	when   []string // when condition for blocks
}

func (e executor) Exec(ctx context.Context) error {
	e.pipeline.Status.Phase = kubekeyv1.PipelinePhaseRunning
	defer func() {
		// update pipeline phase
		e.pipeline.Status.Phase = kubekeyv1.PipelinePhaseSucceed
		if len(e.pipeline.Status.FailedDetail) != 0 {
			e.pipeline.Status.Phase = kubekeyv1.PipelinePhaseFailed
		}
	}()

	klog.V(6).InfoS("deal project", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
	pj, err := project.New(*e.pipeline, true)
	if err != nil {
		klog.V(4).ErrorS(err, "Deal project error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
		return err
	}

	// convert to transfer.Playbook struct
	playbookPath := e.pipeline.Spec.Playbook
	if path.IsAbs(playbookPath) {
		playbookPath = playbookPath[1:]
	}
	pb, err := pj.MarshalPlaybook()
	if err != nil {
		klog.V(4).ErrorS(nil, "convert playbook error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
		return err
	}

	for _, play := range pb.Play {
		if !play.Taggable.IsEnabled(e.pipeline.Spec.Tags, e.pipeline.Spec.SkipTags) {
			// if not match the tags. skip
			continue
		}
		// hosts should contain all host's name. hosts should not be empty.
		var hosts []string
		if ahn, err := e.variable.Get(variable.GetHostnames(play.PlayHost.Hosts)); err == nil {
			hosts = ahn.([]string)
		}
		if len(hosts) == 0 { // if hosts is empty skip this playbook
			klog.V(4).Info("Hosts is empty", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
			continue
		}

		// when gather_fact is set. get host's information from remote.
		if play.GatherFacts {
			for _, h := range hosts {
				gfv, err := getGatherFact(ctx, h, e.variable)
				if err != nil {
					klog.V(4).ErrorS(err, "Get gather fact error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "host", h)
					return err
				}
				// merge host information to runtime variable
				if err := e.variable.Merge(variable.MergeRemoteVariable(h, gfv)); err != nil {
					klog.V(4).ErrorS(err, "Merge gather fact error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "host", h)
					return err
				}
			}
		}

		// Batch execution, with each batch being a group of hosts run in serial.
		var batchHosts [][]string
		if play.RunOnce {
			// runOnce only run in first node
			batchHosts = [][]string{{hosts[0]}}
		} else {
			// group hosts by serial. run the playbook by serial
			batchHosts, err = converter.GroupHostBySerial(hosts, play.Serial.Data)
			if err != nil {
				klog.V(4).ErrorS(err, "Group host by serial error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
				return err
			}
		}

		// generate task by each batch.
		for _, serials := range batchHosts {
			// each batch hosts should not be empty.
			if len(serials) == 0 {
				klog.V(4).ErrorS(nil, "Host is empty", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline))
				return fmt.Errorf("host is empty")
			}

			if err := e.mergeVariable(ctx, e.variable, play.Vars, serials...); err != nil {
				klog.V(4).ErrorS(err, "merge variable error", "pipeline", e.pipeline, "block", play.Name)
				return err
			}

			// generate task from pre tasks
			if err := e.execBlock(ctx, execBlockOptions{
				hosts:  serials,
				blocks: play.PreTasks,
			}); err != nil {
				klog.V(4).ErrorS(err, "Get pre task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "play", play.Name)
				return err
			}

			// generate task from role
			for _, role := range play.Roles {
				if err := e.mergeVariable(ctx, e.variable, role.Vars, serials...); err != nil {
					klog.V(4).ErrorS(err, "merge variable error", "pipeline", e.pipeline, "block", role.Name)
					return err
				}

				if err := e.execBlock(ctx, execBlockOptions{
					hosts:  serials,
					blocks: role.Block,
					role:   role.Role,
					when:   role.When.Data,
				}); err != nil {
					klog.V(4).ErrorS(err, "Get role task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "play", play.Name, "role", role.Role)
					return err
				}
			}
			// generate task from tasks
			if err := e.execBlock(ctx, execBlockOptions{
				hosts:  serials,
				blocks: play.Tasks,
			}); err != nil {
				klog.V(4).ErrorS(err, "Get task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "play", play.Name)
				return err
			}
			// generate task from post tasks
			if err := e.execBlock(ctx, execBlockOptions{
				hosts:  serials,
				blocks: play.Tasks,
			}); err != nil {
				klog.V(4).ErrorS(err, "Get post task from  play error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "play", play.Name)
				return err
			}
		}
	}
	return nil
}

func (e executor) execBlock(ctx context.Context, options execBlockOptions) error {
	for _, at := range options.blocks {
		if !at.Taggable.IsEnabled(e.pipeline.Spec.Tags, e.pipeline.Spec.SkipTags) {
			continue
		}
		hosts := options.hosts
		if at.RunOnce { // only run in first host
			hosts = []string{options.hosts[0]}
		}

		// merge variable which defined in block
		if err := e.mergeVariable(ctx, e.variable, at.Vars, hosts...); err != nil {
			klog.V(5).ErrorS(err, "merge variable error", "pipeline", e.pipeline, "block", at.Name)
			return err
		}

		switch {
		case len(at.Block) != 0:
			// exec block
			if err := e.execBlock(ctx, execBlockOptions{
				hosts:  hosts,
				role:   options.role,
				blocks: at.Block,
				when:   append(options.when, at.When.Data...),
			}); err != nil {
				klog.V(4).ErrorS(err, "Get block task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
				return err
			}

			// if block exec failed exec rescue
			if e.pipeline.Status.Phase == kubekeyv1.PipelinePhaseFailed && len(at.Rescue) != 0 {
				if err := e.execBlock(ctx, execBlockOptions{
					hosts:  hosts,
					blocks: at.Rescue,
					role:   options.role,
					when:   append(options.when, at.When.Data...),
				}); err != nil {
					klog.V(4).ErrorS(err, "Get rescue task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
					return err
				}
			}

			// exec always after block
			if len(at.Always) != 0 {
				if err := e.execBlock(ctx, execBlockOptions{
					hosts:  hosts,
					blocks: at.Always,
					role:   options.role,
					when:   append(options.when, at.When.Data...),
				}); err != nil {
					klog.V(4).ErrorS(err, "Get always task from block error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
					return err
				}
			}

		case at.IncludeTasks != "":
			// include tasks has converted to blocks.
			// do nothing
		default:
			task := converter.MarshalBlock(ctx, options.role, hosts, append(options.when, at.When.Data...), at)
			// complete by pipeline
			task.GenerateName = e.pipeline.Name + "-"
			task.Namespace = e.pipeline.Namespace
			if err := controllerutil.SetControllerReference(e.pipeline, task, e.client.Scheme()); err != nil {
				klog.V(4).ErrorS(err, "Set controller reference error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
				return err
			}
			// complete module by unknown field
			for n, a := range at.UnknownFiled {
				data, err := json.Marshal(a)
				if err != nil {
					klog.V(4).ErrorS(err, "Marshal unknown field error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name, "field", n)
					return err
				}
				if m := modules.FindModule(n); m != nil {
					task.Spec.Module.Name = n
					task.Spec.Module.Args = runtime.RawExtension{Raw: data}
					break
				}
			}
			if task.Spec.Module.Name == "" { // action is necessary for a task
				klog.V(4).ErrorS(nil, "No module/action detected in task", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
				return fmt.Errorf("no module/action detected in task: %s", task.Name)
			}
			// create task
			if err := e.client.Create(ctx, task); err != nil {
				klog.V(4).ErrorS(err, "create task error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
				return err
			}

			for {
				klog.Infof("[Task %s] task exec \"%s\" begin for %v times", ctrlclient.ObjectKeyFromObject(task), task.Spec.Name, task.Status.RestartCount+1)
				// exec task
				task.Status.Phase = kubekeyv1alpha1.TaskPhaseRunning
				if err := e.client.Status().Update(ctx, task); err != nil {
					klog.V(5).ErrorS(err, "update task status error", "task", ctrlclient.ObjectKeyFromObject(task))
				}
				if err := e.executeTask(ctx, task, options); err != nil {
					klog.V(4).ErrorS(err, "exec task error", "pipeline", ctrlclient.ObjectKeyFromObject(e.pipeline), "block", at.Name)
					return err
				}
				if err := e.client.Status().Update(ctx, task); err != nil {
					klog.V(5).ErrorS(err, "update task status error", "task", ctrlclient.ObjectKeyFromObject(task))
					return err
				}

				if task.IsComplete() {
					break
				}
			}
			klog.Infof("[Task %s] task exec \"%s\" end status is %s", ctrlclient.ObjectKeyFromObject(task), task.Spec.Name, task.Status.Phase)
			e.pipeline.Status.TaskResult.Total++
			switch task.Status.Phase {
			case kubekeyv1alpha1.TaskPhaseSuccess:
				e.pipeline.Status.TaskResult.Success++
			case kubekeyv1alpha1.TaskPhaseIgnored:
				e.pipeline.Status.TaskResult.Ignored++
			case kubekeyv1alpha1.TaskPhaseFailed:
				e.pipeline.Status.TaskResult.Failed++
			}

			// exit when task run failed
			if task.IsFailed() {
				var hostReason []kubekeyv1.PipelineFailedDetailHost
				for _, tr := range task.Status.FailedDetail {
					hostReason = append(hostReason, kubekeyv1.PipelineFailedDetailHost{
						Host:   tr.Host,
						Stdout: tr.Stdout,
						StdErr: tr.StdErr,
					})
				}
				e.pipeline.Status.FailedDetail = append(e.pipeline.Status.FailedDetail, kubekeyv1.PipelineFailedDetail{
					Task:  task.Spec.Name,
					Hosts: hostReason,
				})
				e.pipeline.Status.Phase = kubekeyv1.PipelinePhaseFailed
				e.pipeline.Status.Reason = fmt.Sprintf("task %s run failed", task.Name)
				return fmt.Errorf("task %s run failed", task.Name)
			}
		}

	}
	return nil
}

func (e executor) executeTask(ctx context.Context, task *kubekeyv1alpha1.Task, options execBlockOptions) error {
	cd := kubekeyv1alpha1.TaskCondition{
		StartTimestamp: metav1.Now(),
	}
	defer func() {
		cd.EndTimestamp = metav1.Now()
		task.Status.Conditions = append(task.Status.Conditions, cd)
	}()

	// check task host results
	wg := &wait.Group{}
	dataChan := make(chan kubekeyv1alpha1.TaskHostResult, len(task.Spec.Hosts))
	for _, h := range task.Spec.Hosts {
		host := h
		wg.StartWithContext(ctx, func(ctx context.Context) {
			var stdout, stderr string
			defer func() {
				if stderr != "" {
					klog.Errorf("[Task %s] run failed: %s", ctrlclient.ObjectKeyFromObject(task), stderr)
				}

				if task.Spec.Register != "" {
					// set variable to parent location
					if err := e.variable.Merge(variable.MergeRuntimeVariable(host, map[string]any{
						task.Spec.Register: map[string]string{
							"stdout": stdout,
							"stderr": stderr,
						},
					})); err != nil {
						stderr = fmt.Sprintf("register task result to variable error: %v", err)
						return
					}
				}
				// fill result
				dataChan <- kubekeyv1alpha1.TaskHostResult{
					Host:   host,
					Stdout: stdout,
					StdErr: stderr,
				}
			}()

			ha, err := e.variable.Get(variable.GetAllVariable(host))
			if err != nil {
				stderr = fmt.Sprintf("get variable error: %v", err)
				return
			}
			// check when condition
			if len(task.Spec.When) > 0 {
				ok, err := tmpl.ParseBool(ha.(map[string]any), task.Spec.When)
				if err != nil {
					stderr = fmt.Sprintf("parse when condition error: %v", err)
					return
				}
				if !ok {
					stdout = "skip"
					return
				}
			}

			// execute module with loop
			loop, err := e.execLoop(ctx, ha.(map[string]any), task)
			if err != nil {
				stderr = fmt.Sprintf("parse loop vars error: %v", err)
				return
			}

			for _, item := range loop {
				// set item to runtime variable
				if err := e.variable.Merge(variable.MergeRuntimeVariable(host, map[string]any{
					"item": item,
				})); err != nil {
					stderr = fmt.Sprintf("set loop item to variable error: %v", err)
					return
				}
				stdout, stderr = e.executeModule(ctx, task, modules.ExecOptions{
					Args:     task.Spec.Module.Args,
					Host:     host,
					Variable: e.variable,
					Task:     *task,
					Pipeline: *e.pipeline,
				})
				// delete item
				if err := e.variable.Merge(variable.MergeRuntimeVariable(host, map[string]any{
					"item": nil,
				})); err != nil {
					stderr = fmt.Sprintf("clean loop item to variable error: %v", err)
					return
				}
			}
		})
	}
	go func() {
		wg.Wait()
		close(dataChan)
	}()

	task.Status.Phase = kubekeyv1alpha1.TaskPhaseSuccess
	for data := range dataChan {
		if data.StdErr != "" {
			if task.Spec.IgnoreError {
				task.Status.Phase = kubekeyv1alpha1.TaskPhaseIgnored
			} else {
				task.Status.Phase = kubekeyv1alpha1.TaskPhaseFailed
				task.Status.FailedDetail = append(task.Status.FailedDetail, kubekeyv1alpha1.TaskFailedDetail{
					Host:   data.Host,
					Stdout: data.Stdout,
					StdErr: data.StdErr,
				})
			}
		}
		cd.HostResults = append(cd.HostResults, data)
	}

	return nil
}

func (e executor) execLoop(ctx context.Context, ha map[string]any, task *kubekeyv1alpha1.Task) ([]any, error) {
	switch {
	case task.Spec.Loop.Raw == nil:
		// loop is not set. add one element to execute once module.
		return []any{nil}, nil
	default:
		return variable.Extension2Slice(ha, task.Spec.Loop), nil
	}
}

func (e executor) executeModule(ctx context.Context, task *kubekeyv1alpha1.Task, opts modules.ExecOptions) (string, string) {
	lg, err := opts.Variable.Get(variable.GetAllVariable(opts.Host))
	if err != nil {
		klog.V(5).ErrorS(err, "get location variable error", "task", ctrlclient.ObjectKeyFromObject(task))
		return "", err.Error()
	}

	// check failed when condition
	if len(task.Spec.FailedWhen) > 0 {
		ok, err := tmpl.ParseBool(lg.(map[string]any), task.Spec.FailedWhen)
		if err != nil {
			klog.V(5).ErrorS(err, "validate FailedWhen condition error", "task", ctrlclient.ObjectKeyFromObject(task))
			return "", err.Error()
		}
		if ok {
			return "", "failed by failedWhen"
		}
	}

	return modules.FindModule(task.Spec.Module.Name)(ctx, opts)
}

// merge defined variable to host variable
func (e executor) mergeVariable(ctx context.Context, v variable.Variable, vd map[string]any, hosts ...string) error {
	if len(vd) == 0 {
		// skip
		return nil
	}
	for _, host := range hosts {

		if err := v.Merge(variable.MergeRuntimeVariable(host, vd)); err != nil {
			return err
		}
	}
	return nil
}