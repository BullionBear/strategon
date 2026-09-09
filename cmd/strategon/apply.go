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

type natsSpecYAML struct {
	ArtifactVersion    string           `yaml:"artifactVersion"`
	ArtifactVersionAlt string           `yaml:"artifact_version"`
	ConfigVersion      string           `yaml:"configVersion"`
	ConfigVersionAlt   string           `yaml:"config_version"`
	Strategy           string           `yaml:"strategy"`
	Servers            []natsServerYAML `yaml:"servers"`
	Update             natsUpdateYAML   `yaml:"update"`
}

type natsServerYAML struct {
	Machine        string `yaml:"machine"`
	ServerName     string `yaml:"serverName"`
	ServerNameAlt  string `yaml:"server_name"`
	RouteHost      string `yaml:"routeHost"`
	RouteHostAlt   string `yaml:"route_host"`
	ClientPort     int32  `yaml:"clientPort"`
	ClientPortAlt  int32  `yaml:"client_port"`
	ClusterPort    int32  `yaml:"clusterPort"`
	ClusterPortAlt int32  `yaml:"cluster_port"`
	MonitorPort    int32  `yaml:"monitorPort"`
	MonitorPortAlt int32  `yaml:"monitor_port"`
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
	ArtifactVersion    string            `yaml:"artifactVersion"`
	ArtifactVersionAlt string            `yaml:"artifact_version"`
	ConfigVersion      string            `yaml:"configVersion"`
	ConfigVersionAlt   string            `yaml:"config_version"`
	Stopped            bool              `yaml:"stopped"`
	Args               []string          `yaml:"args"`
	Env                map[string]string `yaml:"env"`
	DeployPolicy       *policyYAML       `yaml:"deployPolicy"`
	DeployPolicyAlt    *policyYAML       `yaml:"deploy_policy"`
	Schedules          []scheduleYAML    `yaml:"schedules"`
	Limits             *limitsYAML       `yaml:"limits"`
	Lease              *leaseYAML        `yaml:"lease"`
	Readiness          *readinessYAML    `yaml:"readiness"`
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

type readinessYAML struct {
	Endpoint string `yaml:"endpoint"`
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
	case "natscluster":
		return applyNatsCluster(ctx, client, doc)
	case "strategyassignment":
		return applyAssignment(ctx, client, doc)
	case "":
		return ApplyResult{}, fmt.Errorf("missing kind")
	default:
		return ApplyResult{}, fmt.Errorf("unknown kind %q: expected NatsCluster or StrategyAssignment", kind)
	}
}

func applyNatsCluster(ctx context.Context, client strategyplatformv1connect.ControlPlaneServiceClient, doc yamlDoc) (ApplyResult, error) {
	name := strings.TrimSpace(doc.Metadata.Name)
	if name == "" {
		return ApplyResult{}, fmt.Errorf("NatsCluster metadata.name is required")
	}
	var spec natsSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		return ApplyResult{}, fmt.Errorf("NatsCluster spec: %w", err)
	}
	servers := make([]*pb.NatsServer, 0, len(spec.Servers))
	for _, s := range spec.Servers {
		servers = append(servers, &pb.NatsServer{
			Machine:     s.Machine,
			ServerName:  firstNonEmpty(s.ServerName, s.ServerNameAlt),
			RouteHost:   firstNonEmpty(s.RouteHost, s.RouteHostAlt),
			ClientPort:  pickInt32(s.ClientPort, s.ClientPortAlt),
			ClusterPort: pickInt32(s.ClusterPort, s.ClusterPortAlt),
			MonitorPort: pickInt32(s.MonitorPort, s.MonitorPortAlt),
		})
	}
	req := &pb.ApplyNatsClusterRequest{
		Cluster: &pb.NatsCluster{
			Metadata: &pb.ObjectMeta{Name: name, Labels: doc.Metadata.Labels},
			Spec: &pb.NatsClusterSpec{
				ArtifactVersion: firstNonEmpty(spec.ArtifactVersion, spec.ArtifactVersionAlt),
				ConfigVersion:   firstNonEmpty(spec.ConfigVersion, spec.ConfigVersionAlt),
				Strategy:        spec.Strategy,
				Servers:         servers,
				Update: &pb.NatsClusterUpdate{
					MaxUnavailable:   pickInt32(spec.Update.MaxUnavailable, spec.Update.MaxUnavailableAlt),
					WaitReadySeconds: pickInt32(spec.Update.WaitReadySeconds, spec.Update.WaitReadySecondsAlt),
				},
			},
		},
	}
	resp, err := client.ApplyNatsCluster(ctx, connect.NewRequest(req))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("ApplyNatsCluster %s: %w", name, err)
	}
	return ApplyResult{
		Kind:       "NatsCluster",
		Name:       name,
		RPC:        "ApplyNatsCluster",
		Generation: resp.Msg.GetCluster().GetMetadata().GetGeneration(),
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
