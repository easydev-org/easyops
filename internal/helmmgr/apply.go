package helmmgr

import (
	"context"
	"fmt"
	"time"
)

// ---- apply ----

// Apply renders + applies a chart as a NEW or updated release. It records a
// new revision in the release history.
func (c *Client) Apply(ctx context.Context, chartDir string, o RenderOptions, creator, session string) (Release, error) {
	if !validReleaseName(o.Release) {
		return Release{}, fmt.Errorf("release name must be a DNS-1123 label")
	}
	manifest, objects, chartVersion, appVersion, err := c.Template(chartDir, o)
	if err != nil {
		return Release{}, err
	}
	if err := c.applyManifest(ctx, manifest, o.Release); err != nil {
		return Release{}, err
	}
	cur, ok, err := c.Get(ctx, o.Release)
	if err != nil {
		return Release{}, err
	}
	if !ok {
		cur = Release{Name: o.Release, Namespace: c.namespace, Creator: creator, Session: session, OrgNS: o.OrgNS}
	}
	cur.Ref = o.Ref
	cur.ChartPath = o.ChartPath
	cur.ChartVersion = chartVersion
	cur.AppVersion = appVersion
	cur.Revision++
	cur.Status = "deployed"
	cur.UpdatedAt = time.Now().UnixMilli()
	cur.History = append(cur.History, Revision{
		Revision: cur.Revision, Ref: o.Ref, ChartPath: o.ChartPath,
		ChartVersion: chartVersion, AppVersion: appVersion, Values: o.ValuesYAML,
		CreatedAt: cur.UpdatedAt, Objects: objects, Manifest: manifest,
	})
	if err := c.put(ctx, cur); err != nil {
		return Release{}, err
	}
	return cur, nil
}
