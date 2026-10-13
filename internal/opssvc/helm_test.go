package opssvc

import (
	"context"
	"testing"

	"github.com/easy-platform/easyops/internal/helmmgr"
)

// TestHelmReleaseInfoChartAndObjects checks the list-view enrichment: chart/
// app version and the CURRENT revision's objects are mapped onto HelmReleaseInfo.
func TestHelmReleaseInfoChartAndObjects(t *testing.T) {
	s := &Service{} // no helm backend needed for a plain release
	rel := helmmgr.Release{
		Name: "app", Namespace: "worker", Creator: "alice",
		ChartVersion: "1.2.3", AppVersion: "4.5.6", Revision: 2,
		History: []helmmgr.Revision{
			{Revision: 1, Objects: []helmmgr.Object{{Kind: "ConfigMap", Name: "old"}}},
			{Revision: 2, Objects: []helmmgr.Object{
				{Kind: "Deployment", Name: "app"},
				{Kind: "Service", Name: "app"},
			}},
		},
	}
	info := s.helmReleaseInfo(context.Background(), rel)
	if info.GetChartVersion() != "1.2.3" || info.GetAppVersion() != "4.5.6" {
		t.Fatalf("versions = %q/%q", info.GetChartVersion(), info.GetAppVersion())
	}
	objs := info.GetObjects()
	if len(objs) != 2 || objs[0] != "Deployment/app" || objs[1] != "Service/app" {
		t.Fatalf("objects = %v (want the CURRENT revision's)", objs)
	}
}

// TestCurrentObjectsEmpty checks a release with no history yields no objects.
func TestCurrentObjectsEmpty(t *testing.T) {
	if got := currentObjects(helmmgr.Release{}); got != nil {
		t.Fatalf("no history must yield nil, got %v", got)
	}
}
