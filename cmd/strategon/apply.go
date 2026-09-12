package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"gopkg.in/yaml.v3"
)

// ApplyResult is one document applied by apply -f.
type ApplyResult struct {
	Kind       string
	Name       string
	RPC        string
	Generation int64
}

type yamlDoc struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   yamlMeta  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

type yamlMeta struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}

type setSpecYAML struct {
	ArtifactVersion    string          `yaml:"artifactVersion"`
	ArtifactVersionAlt string          `yaml:"artifact_version"`
	ConfigVersion      string          `yaml:"configVersion"`
	ConfigVersionAlt   string          `yaml:"config_version"`
	Strategy           string          `yaml:"strategy"`
	Template           memberTmplYAML  `yaml:"template"`
	Members            []setMemberYAML `yaml:"members"`
	Update             natsUpdateYAML  `yaml:"update"`
}

type memberTmplYAML struct {
	Args            []string          `yaml:"args"`
	Env             map[string]string `yaml:"env"`
	Readiness       readinessYAML     `yaml:"readiness"`
	DeployPolicy    *deployPolicyYAML `yaml:"deployPolicy"`
	Peers           peersYAML         `yaml:"peers"`
	VolumeMounts    []volumeMountYAML `yaml:"volumeMounts"`
	CaptureStdio    bool              `yaml:"captureStdio"`
	CaptureStdioAlt bool              `yaml:"capture_stdio"`
}

type readinessYAML struct {
	Endpoint string `yaml:"endpoint"`
}

type deployPolicyYAML struct {
	Startsecs           int32 `yaml:"startsecs"`
	HealthWindowSeconds int32 `yaml:"healthWindowSeconds"`
	MaxCrashesInWindow  int32 `yaml:"maxCrashesInWindow"`
	StopGraceSeconds    int32 `yaml:"stopGraceSeconds"`
	EnableAutoRollback  *bool `yaml:"enableAutoRollback"`
}

type peersYAML struct {
	Format      string `yaml:"format"`
	Separator   string `yaml:"separator"`
	IncludeSelf bool   `yaml:"includeSelf"`
}

type setMemberYAML struct {
	Machine string            `yaml:"machine"`
	Name    string            `yaml:"name"`
	Vars    map[string]string `yaml:"vars"`
}

type natsUpdateYAML struct {
	MaxUnavailable      int32 `yaml:"maxUnavailable"`
	MaxUnavailableAlt   int32 `yaml:"max_unavailable"`
	WaitReadySeconds    int32 `yaml:"waitReadySeconds"`
	WaitReadySecondsAlt int32 `yaml:"wait_ready_seconds"`
}

type assignmentSpecYAML struct {
	MachineID          string            `yaml:"machineId"`
	MachineIDAlt       string            `yaml:"machine_id"`
	Strategy           string            `yaml:"strategy"`
	Artifact           string            `yaml:"artifact"`
	ArtifactVersion    string            `yaml:"artifactVersion"`
	ArtifactVersionAlt string            `yaml:"artifact_version"`
	ConfigVersion      string            `yaml:"configVersion"`
	ConfigVersionAlt   string            `yaml:"config_version"`
	Stopped            bool              `yaml:"stopped"` // omit/false = should be running
	Args               []string          `yaml:"args"`
	Env                map[string]string `yaml:"env"`
	DeployPolicy       *policyYAML       `yaml:"deployPolicy"`
	DeployPolicyAlt    *policyYAML       `yaml:"deploy_policy"`
	Schedules          []scheduleYAML    `yaml:"schedules"`
	Limits             *limitsYAML       `yaml:"limits"`
	Lease              *leaseYAML        `yaml:"lease"`
	Readiness          *readinessYAML    `yaml:"readiness"`
	VolumeMounts       []volumeMountYAML `yaml:"volumeMounts"`
	// Apply is authoritative: omit / false turns payload stdio capture off
	// (same as stopped — not carried forward like empty configVersion).
	CaptureStdio    bool `yaml:"captureStdio"`
	CaptureStdioAlt bool `yaml:"capture_stdio"`
}

type volumeMountYAML struct {
	Name             string `yaml:"name"`
	ContainerPath    string `yaml:"containerPath"`
	ContainerPathAlt string `yaml:"container_path"`
}

type machineVolumesSpecYAML struct {
	MachineID    string           `yaml:"machineId"`
	MachineIDAlt string           `yaml:"machine_id"`
	Volumes      []volumeSpecYAML `yaml:"volumes"`
}

type volumeSpecYAML struct {
	Name string `yaml:"name"`
}

type policyYAML struct {
	Startsecs              int32 `yaml:"startsecs"`
	HealthWindowSeconds    int32 `yaml:"healthWindowSeconds"`
	HealthWindowSecondsAlt int32 `yaml:"health_window_seconds"`
	MaxCrashesInWindow     int32 `yaml:"maxCrashesInWindow"`
	MaxCrashesInWindowAlt  int32 `yaml:"max_crashes_in_window"`
	StopGraceSeconds       int32 `yaml:"stopGraceSeconds"`
	StopGraceSecondsAlt    int32 `yaml:"stop_grace_seconds"`
	EnableAutoRollback     bool  `yaml:"enableAutoRollback"`
	EnableAutoRollbackAlt  *bool `yaml:"enable_auto_rollback"`
}

type scheduleYAML struct {
	Name             string `yaml:"name"`
	CronExpr         string `yaml:"cronExpr"`
	CronExprAlt      string `yaml:"cron_expr"`
	Timezone         string `yaml:"timezone"`
	Action           string `yaml:"action"`
	JitterSeconds    int32  `yaml:"jitterSeconds"`
	JitterSecondsAlt int32  `yaml:"jitter_seconds"`
	ScriptRef        string `yaml:"scriptRef"`
	ScriptRefAlt     string `yaml:"script_ref"`
}

type limitsYAML struct {
	CPUMillicores    int64 `yaml:"cpuMillicores"`
	CPUMillicoresAlt int64 `yaml:"cpu_millicores"`
	MemoryBytes      int64 `yaml:"memoryBytes"`
	MemoryBytesAlt   int64 `yaml:"memory_bytes"`
	MaxOpenFiles     int32 `yaml:"maxOpenFiles"`
	MaxOpenFilesAlt  int32 `yaml:"max_open_files"`
}

type leaseYAML struct {
	Required      bool  `yaml:"required"`
	TTLSeconds    int32 `yaml:"ttlSeconds"`
	TTLSecondsAlt int32 `yaml:"ttl_seconds"`
}

func applyFile(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, path string) ([]ApplyResult, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	return applyReader(ctx, client, r)
}

func applyReader(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, r io.Reader) ([]ApplyResult, error) {
	dec := yaml.NewDecoder(r)
	var out []ApplyResult
	for {
		var doc yamlDoc
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return out, fmt.Errorf("parse yaml: %w", err)
		}
		if strings.TrimSpace(doc.Kind) == "" && doc.Spec.Kind == 0 && doc.Metadata.Name == "" {
			continue
		}
		res, err := applyDoc(ctx, client, doc)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no documents in file")
	}
	return out, nil
}

func applyDoc(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, doc yamlDoc) (ApplyResult, error) {
	kind := strings.TrimSpace(doc.Kind)
	switch strings.ToLower(kind) {
	case "assignmentset":
		return applyAssignmentSet(ctx, client, doc)
	case "strategyassignment":
		return applyAssignment(ctx, client, doc)
	case "machinevolumes":
		return applyMachineVolumes(ctx, client, doc)
	case "":
		return ApplyResult{}, fmt.Errorf("missing kind")
	default:
		return ApplyResult{}, fmt.Errorf("unknown kind %q: expected AssignmentSet, StrategyAssignment, or MachineVolumes", kind)
	}
}

func applyAssignmentSet(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, doc yamlDoc) (ApplyResult, error) {
	name := strings.TrimSpace(doc.Metadata.Name)
	if name == "" {
		return ApplyResult{}, fmt.Errorf("AssignmentSet metadata.name is required")
	}
	var spec setSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		return ApplyResult{}, fmt.Errorf("AssignmentSet spec: %w", err)
	}
	members := make([]*pb.SetMember, 0, len(spec.Members))
	for _, m := range spec.Members {
		members = append(members, &pb.SetMember{
			Machine: m.Machine,
			Name:    m.Name,
			Vars:    m.Vars,
		})
	}
	tmpl := &pb.MemberTemplate{
		Args:         spec.Template.Args,
		Env:          spec.Template.Env,
		VolumeMounts: protoMounts(spec.Template.VolumeMounts),
		CaptureStdio: spec.Template.CaptureStdio || spec.Template.CaptureStdioAlt,
	}
	if spec.Template.Readiness.Endpoint != "" {
		tmpl.Readiness = &pb.ReadinessProbe{Endpoint: spec.Template.Readiness.Endpoint}
	}
	if spec.Template.Peers.Format != "" {
		tmpl.Peers = &pb.PeerList{
			Format:      spec.Template.Peers.Format,
			Separator:   spec.Template.Peers.Separator,
			IncludeSelf: spec.Template.Peers.IncludeSelf,
		}
	}
	if p := spec.Template.DeployPolicy; p != nil {
		tmpl.DeployPolicy = &pb.DeployPolicy{
			Startsecs:           p.Startsecs,
			HealthWindowSeconds: p.HealthWindowSeconds,
			MaxCrashesInWindow:  p.MaxCrashesInWindow,
			StopGraceSeconds:    p.StopGraceSeconds,
			EnableAutoRollback:  p.EnableAutoRollback == nil || *p.EnableAutoRollback,
		}
	}
	req := &pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: name, Labels: doc.Metadata.Labels},
			Spec: &pb.AssignmentSetSpec{
				ArtifactVersion: firstNonEmpty(spec.ArtifactVersion, spec.ArtifactVersionAlt),
				ConfigVersion:   firstNonEmpty(spec.ConfigVersion, spec.ConfigVersionAlt),
				Strategy:        spec.Strategy,
				Template:        tmpl,
				Members:         members,
				Update: &pb.RollingUpdate{
					MaxUnavailable:   pickInt32(spec.Update.MaxUnavailable, spec.Update.MaxUnavailableAlt),
					WaitReadySeconds: pickInt32(spec.Update.WaitReadySeconds, spec.Update.WaitReadySecondsAlt),
				},
			},
		},
	}
	resp, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(req))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("ApplyAssignmentSet %s: %w", name, err)
	}
	return ApplyResult{
		Kind:       "AssignmentSet",
		Name:       name,
		RPC:        "ApplyAssignmentSet",
		Generation: resp.Msg.GetSet().GetMetadata().GetGeneration(),
	}, nil
}

func applyAssignment(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, doc yamlDoc) (ApplyResult, error) {
	var spec assignmentSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		return ApplyResult{}, fmt.Errorf("StrategyAssignment spec: %w", err)
	}
	name := firstNonEmpty(doc.Metadata.Name, spec.Strategy)
	machine := firstNonEmpty(spec.MachineID, spec.MachineIDAlt)
	if name == "" {
		return ApplyResult{}, fmt.Errorf("StrategyAssignment metadata.name (strategy) is required")
	}
	if spec.Strategy != "" && spec.Strategy != name {
		return ApplyResult{}, fmt.Errorf("StrategyAssignment metadata.name %q does not match spec.strategy %q", name, spec.Strategy)
	}
	if machine == "" {
		return ApplyResult{}, fmt.Errorf("StrategyAssignment spec.machineId is required")
	}
	artVer := firstNonEmpty(spec.ArtifactVersion, spec.ArtifactVersionAlt)
	if artVer == "" {
		return ApplyResult{}, fmt.Errorf("StrategyAssignment spec.artifactVersion is required")
	}
	req := &pb.ApplyAssignmentRequest{
		MachineId:       machine,
		Strategy:        name,
		Artifact:        spec.Artifact,
		ArtifactVersion: artVer,
		ConfigVersion:   firstNonEmpty(spec.ConfigVersion, spec.ConfigVersionAlt),
		Stopped:         spec.Stopped,
		Args:            append([]string(nil), spec.Args...),
		Env:             spec.Env,
		DeployPolicy:    spec.policy(),
		Schedules:       spec.schedules(),
		Limits:          spec.limits(),
		Lease:           spec.lease(),
		Readiness:       spec.readiness(),
		VolumeMounts:    protoMounts(spec.VolumeMounts),
		CaptureStdio:    spec.CaptureStdio || spec.CaptureStdioAlt,
	}
	resp, err := client.ApplyAssignment(ctx, connect.NewRequest(req))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("ApplyAssignment %s/%s: %w", machine, name, err)
	}
	return ApplyResult{
		Kind:       "StrategyAssignment",
		Name:       name,
		RPC:        "ApplyAssignment",
		Generation: resp.Msg.GetGeneration(),
	}, nil
}

func (s assignmentSpecYAML) policy() *pb.DeployPolicy {
	p := s.DeployPolicy
	if p == nil {
		p = s.DeployPolicyAlt
	}
	if p == nil {
		return nil
	}
	enable := p.EnableAutoRollback
	if p.EnableAutoRollbackAlt != nil {
		enable = *p.EnableAutoRollbackAlt
	}
	return &pb.DeployPolicy{
		Startsecs:           p.Startsecs,
		HealthWindowSeconds: pickInt32(p.HealthWindowSeconds, p.HealthWindowSecondsAlt),
		MaxCrashesInWindow:  pickInt32(p.MaxCrashesInWindow, p.MaxCrashesInWindowAlt),
		StopGraceSeconds:    pickInt32(p.StopGraceSeconds, p.StopGraceSecondsAlt),
		EnableAutoRollback:  enable,
	}
}

func (s assignmentSpecYAML) schedules() []*pb.CronSchedule {
	if len(s.Schedules) == 0 {
		return nil
	}
	out := make([]*pb.CronSchedule, 0, len(s.Schedules))
	for _, sch := range s.Schedules {
		out = append(out, &pb.CronSchedule{
			Name:          sch.Name,
			CronExpr:      firstNonEmpty(sch.CronExpr, sch.CronExprAlt),
			Timezone:      sch.Timezone,
			Action:        parseCronAction(sch.Action),
			JitterSeconds: pickInt32(sch.JitterSeconds, sch.JitterSecondsAlt),
			ScriptRef:     firstNonEmpty(sch.ScriptRef, sch.ScriptRefAlt),
		})
	}
	return out
}

func (s assignmentSpecYAML) limits() *pb.ResourceLimits {
	if s.Limits == nil {
		return nil
	}
	l := s.Limits
	return &pb.ResourceLimits{
		CpuMillicores: pickInt64(l.CPUMillicores, l.CPUMillicoresAlt),
		MemoryBytes:   pickInt64(l.MemoryBytes, l.MemoryBytesAlt),
		MaxOpenFiles:  pickInt32(l.MaxOpenFiles, l.MaxOpenFilesAlt),
	}
}

func (s assignmentSpecYAML) lease() *pb.LeaseSpec {
	if s.Lease == nil {
		return nil
	}
	return &pb.LeaseSpec{
		Required:   s.Lease.Required,
		TtlSeconds: pickInt32(s.Lease.TTLSeconds, s.Lease.TTLSecondsAlt),
	}
}

func (s assignmentSpecYAML) readiness() *pb.ReadinessProbe {
	if s.Readiness == nil || strings.TrimSpace(s.Readiness.Endpoint) == "" {
		return nil
	}
	return &pb.ReadinessProbe{Endpoint: s.Readiness.Endpoint}
}

func parseCronAction(s string) pb.CronAction {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "RESTART", "CRON_ACTION_RESTART":
		return pb.CronAction_CRON_ACTION_RESTART
	case "RELOAD_CONFIG", "CRON_ACTION_RELOAD_CONFIG":
		return pb.CronAction_CRON_ACTION_RELOAD_CONFIG
	case "RUN_SCRIPT", "CRON_ACTION_RUN_SCRIPT":
		return pb.CronAction_CRON_ACTION_RUN_SCRIPT
	default:
		return pb.CronAction_CRON_ACTION_UNSPECIFIED
	}
}

func pickInt32(a, b int32) int32 {
	if a != 0 {
		return a
	}
	return b
}

func pickInt64(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func formatApply(res ApplyResult) string {
	return fmt.Sprintf("%s/%s applied via %s generation=%d", res.Kind, res.Name, res.RPC, res.Generation)
}

func protoMounts(in []volumeMountYAML) []*pb.VolumeMount {
	if len(in) == 0 {
		return nil
	}
	out := make([]*pb.VolumeMount, 0, len(in))
	for _, m := range in {
		out = append(out, &pb.VolumeMount{
			Name:          m.Name,
			ContainerPath: firstNonEmpty(m.ContainerPath, m.ContainerPathAlt),
		})
	}
	return out
}

func applyMachineVolumes(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, doc yamlDoc) (ApplyResult, error) {
	var spec machineVolumesSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		return ApplyResult{}, fmt.Errorf("MachineVolumes spec: %w", err)
	}
	machine := firstNonEmpty(spec.MachineID, spec.MachineIDAlt, doc.Metadata.Name)
	if machine == "" {
		return ApplyResult{}, fmt.Errorf("MachineVolumes metadata.name (machine id) is required")
	}
	if len(spec.Volumes) == 0 {
		return ApplyResult{}, fmt.Errorf("MachineVolumes spec.volumes is required")
	}
	var lastGen int64
	for _, v := range spec.Volumes {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			return ApplyResult{}, fmt.Errorf("MachineVolumes volume name is required")
		}
		resp, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
			MachineId: machine,
			Name:      name,
		}))
		if err != nil {
			return ApplyResult{}, fmt.Errorf("CreateVolume %s/%s: %w", machine, name, err)
		}
		lastGen = resp.Msg.GetGeneration()
	}
	return ApplyResult{
		Kind:       "MachineVolumes",
		Name:       machine,
		RPC:        "CreateVolume",
		Generation: lastGen,
	}, nil
}
