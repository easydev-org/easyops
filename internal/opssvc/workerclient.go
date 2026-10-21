package opssvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/easy-platform/easyops/k8s/sandboxreaper"
)

// WorkerClient is a minimal agent-worker client. It speaks the worker's Connect
// endpoints (`/worker.v1.WorkerService/...`) over the JSON codec, so the easyops
// library stays free of the worker protocol (no gen/worker, no connectrpc).
//
// It is used by the idle-sandbox reaper (ListJobs) and by the sandbox package
// bootstrap (Execute + JobWait).
type WorkerClient struct {
	url   string
	token string
	// hc bounds the short, control-plane calls (ListJobs).
	hc *http.Client
	// jobHC is used for Execute/JobWait, whose duration is bounded by the
	// caller's context (a job may legitimately run for minutes), so it must NOT
	// carry a fixed client timeout that would truncate a long wait.
	jobHC *http.Client
}

// NewWorkerClient builds a bearer-authenticated worker client for url.
func NewWorkerClient(url, token string) *WorkerClient {
	return &WorkerClient{
		url:   strings.TrimRight(url, "/"),
		token: token,
		hc:    &http.Client{Timeout: 10 * time.Second},
		jobHC: &http.Client{},
	}
}

// post issues a JSON-codec Connect unary call to `procedure` and decodes the
// response into out. A non-2xx status is an error.
func (c *WorkerClient) post(ctx context.Context, hc *http.Client, procedure string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+procedure, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("worker %s: HTTP %d", procedure, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- jobs (idle reclamation) ----

// workerJobsResponse mirrors the worker's ListJobs response (JSON codec; int64
// timestamps are encoded as strings per proto3 JSON).
type workerJobsResponse struct {
	Jobs []struct {
		State      string `json:"state"`
		FinishedAt any    `json:"finishedAt"`
	} `json:"jobs"`
}

// ListJobs returns the worker's job history as sandboxreaper.Job values.
func (c *WorkerClient) ListJobs(ctx context.Context) ([]sandboxreaper.Job, error) {
	var out workerJobsResponse
	if err := c.post(ctx, c.hc, "/worker.v1.WorkerService/ListJobs", struct{}{}, &out); err != nil {
		return nil, err
	}
	jobs := make([]sandboxreaper.Job, 0, len(out.Jobs))
	for _, j := range out.Jobs {
		jobs = append(jobs, sandboxreaper.Job{State: j.State, FinishedAt: asInt64(j.FinishedAt)})
	}
	return jobs, nil
}

// ---- execute + wait (package bootstrap) ----

// workerInfoResponse mirrors the subset of worker.v1.InfoResponse we need.
type workerInfoResponse struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Home   string `json:"home"`
	BootID string `json:"bootId"`
}

// Info returns the worker's OS/home. Used by the package bootstrap to pick the
// OS-specific config set (linux / darwin / windows).
func (c *WorkerClient) Info(ctx context.Context) (workerInfo, error) {
	var out workerInfoResponse
	if err := c.post(ctx, c.hc, "/worker.v1.WorkerService/Info", struct{}{}, &out); err != nil {
		return workerInfo{}, err
	}
	return workerInfo{OS: out.OS, Home: out.Home}, nil
}

// workerFileWriteRequest mirrors worker.v1.FileWriteRequest. Content is bytes,
// which proto3 JSON encodes as base64.
type workerFileWriteRequest struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// WriteFile writes `content` to `path` in the sandbox via the worker's file
// service (FileWrite). The worker creates missing parent directories and the
// write is idempotent (overwrite). It needs NO shell, so it works on any
// sandbox image (unlike a job).
func (c *WorkerClient) WriteFile(ctx context.Context, path string, content []byte) error {
	return c.post(ctx, c.hc, "/worker.v1.WorkerService/FileWrite",
		workerFileWriteRequest{Path: path, Content: content}, nil)
}

// workerExecuteRequest mirrors worker.v1.ExecuteRequest. proto3 JSON uses the
// lowerCamelCase field names (timeoutMs, not timeout_ms).
type workerExecuteRequest struct {
	Command   string            `json:"command"`
	Workdir   string            `json:"workdir,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMs int32             `json:"timeoutMs,omitempty"`
}

type workerExecuteResponse struct {
	JobID string `json:"jobId"`
}

type workerJobWaitRequest struct {
	JobID     string `json:"jobId"`
	TimeoutMs int32  `json:"timeoutMs,omitempty"`
}

type workerJobWaitResponse struct {
	State    string `json:"state"`
	ExitCode int32  `json:"exitCode"`
}

// Execute starts `command` in the sandbox worker and returns its job id. It does
// NOT wait for completion (use JobWait). `env` entries are merged onto the
// worker's inherited environment.
func (c *WorkerClient) Execute(ctx context.Context, command, workdir string, env map[string]string, timeoutMs int32) (string, error) {
	var out workerExecuteResponse
	if err := c.post(ctx, c.jobHC, "/worker.v1.WorkerService/Execute", workerExecuteRequest{
		Command: command, Workdir: workdir, Env: env, TimeoutMs: timeoutMs,
	}, &out); err != nil {
		return "", err
	}
	if out.JobID == "" {
		return "", fmt.Errorf("worker Execute returned an empty job id")
	}
	return out.JobID, nil
}

// JobWait blocks until the job completes or timeoutMs elapses, returning the
// terminal state ("done"/"killed"/"failed") and exit code. A state of "running"
// means the deadline expired, not that the job failed.
func (c *WorkerClient) JobWait(ctx context.Context, jobID string, timeoutMs int32) (string, int32, error) {
	var out workerJobWaitResponse
	if err := c.post(ctx, c.jobHC, "/worker.v1.WorkerService/JobWait", workerJobWaitRequest{
		JobID: jobID, TimeoutMs: timeoutMs,
	}, &out); err != nil {
		return "", 0, err
	}
	return out.State, out.ExitCode, nil
}

// asInt64 accepts a proto3-JSON int64 (string or number) and returns 0 otherwise.
func asInt64(v any) int64 {
	switch t := v.(type) {
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	case float64:
		return int64(t)
	}
	return 0
}
