package opssvc

import (
	"context"

	"connectrpc.com/connect/v2"

	opsv1 "github.com/easy-platform/easyops/gen/easyops/v1"
	"github.com/easy-platform/easyops/internal/servicesmgr"
)

// ---- configmaps ----

// PutConfigMap creates/updates a ConfigMap owned by `owner`. A1: an existing
// ConfigMap not writable by `owner` (unowned or another tenant) is refused.
func (s *Service) PutConfigMap(ctx context.Context, req *opsv1.PutConfigMapRequest) (*opsv1.PutConfigMapResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	m := req
	if m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	if m.GetDryRun() {
		return &opsv1.PutConfigMapResponse{}, nil
	}
	cm, err := s.services.PutConfigMap(ctx, m.GetName(), m.GetData(), m.GetOwner(), m.GetNamespace())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.PutConfigMapResponse{Configmap: toConfigMapInfo(cm)}, nil
}

// ListConfigMaps lists the managed ConfigMaps visible to `owner` (unowned
// visible; another tenant's hidden).
func (s *Service) ListConfigMaps(ctx context.Context, req *opsv1.ListConfigMapsRequest) (*opsv1.ListConfigMapsResponse, error) {
	if s.services == nil {
		return &opsv1.ListConfigMapsResponse{}, nil
	}
	all, err := s.services.ListConfigMaps(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.ConfigMap, 0, len(all))
	for _, cm := range all {
		if !owned(cm.Creator, req.GetOwner()) {
			continue
		}
		out = append(out, toConfigMapInfo(cm))
	}
	return &opsv1.ListConfigMapsResponse{Configmaps: out}, nil
}

// DeleteConfigMap removes a ConfigMap writable by `owner` (A1).
func (s *Service) DeleteConfigMap(ctx context.Context, req *opsv1.DeleteConfigMapRequest) (*opsv1.DeleteConfigMapResponse, error) {
	if s.services == nil {
		return &opsv1.DeleteConfigMapResponse{Deleted: false}, nil
	}
	cur, err := s.services.GetConfigMap(ctx, req.GetName())
	if err != nil || !writable(cur.Creator, req.GetOwner()) {
		return &opsv1.DeleteConfigMapResponse{Deleted: false}, nil
	}
	ok, err := s.services.DeleteConfigMap(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.DeleteConfigMapResponse{Deleted: ok}, nil
}

func toConfigMapInfo(cm servicesmgr.ConfigMap) *opsv1.ConfigMap {
	return &opsv1.ConfigMap{
		Name: cm.Name, Data: cm.Data, Owner: cm.Creator,
		CreatedAt: cm.CreatedAt, Namespace: cm.Namespace,
	}
}

// ---- secrets ----

// PutSecret creates/updates a Secret owned by `owner` (A1 applied by the lib).
func (s *Service) PutSecret(ctx context.Context, req *opsv1.PutSecretRequest) (*opsv1.PutSecretResponse, error) {
	if s.services == nil {
		return nil, errNoServiceBackend()
	}
	m := req
	if m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	if m.GetDryRun() {
		return &opsv1.PutSecretResponse{}, nil
	}
	sec, err := s.services.PutSecret(ctx, m.GetName(), m.GetData(), m.GetOwner(), m.GetNamespace())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.PutSecretResponse{Secret: toSecretInfo(sec)}, nil
}

// ListSecrets lists the managed Secrets visible to `owner`.
func (s *Service) ListSecrets(ctx context.Context, req *opsv1.ListSecretsRequest) (*opsv1.ListSecretsResponse, error) {
	if s.services == nil {
		return &opsv1.ListSecretsResponse{}, nil
	}
	all, err := s.services.ListSecrets(ctx)
	if err != nil {
		return nil, mapK8sErr(err)
	}
	out := make([]*opsv1.Secret, 0, len(all))
	for _, sec := range all {
		if !owned(sec.Creator, req.GetOwner()) {
			continue
		}
		out = append(out, toSecretInfo(sec))
	}
	return &opsv1.ListSecretsResponse{Secrets: out}, nil
}

// DeleteSecret removes a Secret writable by `owner` (A1).
func (s *Service) DeleteSecret(ctx context.Context, req *opsv1.DeleteSecretRequest) (*opsv1.DeleteSecretResponse, error) {
	if s.services == nil {
		return &opsv1.DeleteSecretResponse{Deleted: false}, nil
	}
	cur, err := s.services.GetSecret(ctx, req.GetName())
	if err != nil || !writable(cur.Creator, req.GetOwner()) {
		return &opsv1.DeleteSecretResponse{Deleted: false}, nil
	}
	ok, err := s.services.DeleteSecret(ctx, req.GetName())
	if err != nil {
		return nil, mapK8sErr(err)
	}
	return &opsv1.DeleteSecretResponse{Deleted: ok}, nil
}

func toSecretInfo(sec servicesmgr.Secret) *opsv1.Secret {
	return &opsv1.Secret{
		Name: sec.Name, Data: sec.Data, Owner: sec.Creator,
		CreatedAt: sec.CreatedAt, Namespace: sec.Namespace,
	}
}

// GetConfigMap returns one ConfigMap visible to `owner`.
func (s *Service) GetConfigMap(ctx context.Context, req *opsv1.GetConfigMapRequest) (*opsv1.GetConfigMapResponse, error) {
	if s.services == nil {
		return &opsv1.GetConfigMapResponse{Found: false}, nil
	}
	cm, err := s.services.GetConfigMap(ctx, req.GetName())
	if err != nil || !owned(cm.Creator, req.GetOwner()) {
		return &opsv1.GetConfigMapResponse{Found: false}, nil
	}
	return &opsv1.GetConfigMapResponse{Configmap: toConfigMapInfo(cm), Found: true}, nil
}

// GetSecret returns one Secret visible to `owner`.
func (s *Service) GetSecret(ctx context.Context, req *opsv1.GetSecretRequest) (*opsv1.GetSecretResponse, error) {
	if s.services == nil {
		return &opsv1.GetSecretResponse{Found: false}, nil
	}
	sec, err := s.services.GetSecret(ctx, req.GetName())
	if err != nil || !owned(sec.Creator, req.GetOwner()) {
		return &opsv1.GetSecretResponse{Found: false}, nil
	}
	return &opsv1.GetSecretResponse{Secret: toSecretInfo(sec), Found: true}, nil
}
