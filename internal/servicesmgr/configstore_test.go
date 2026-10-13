package servicesmgr

import (
	"context"
	"testing"
)

func TestConfigMapPutGetListDelete(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	cm, err := c.PutConfigMap(ctx, "cfg", map[string]string{"a.conf": "x"}, "alice", "acme")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if cm.Creator != "alice" || cm.Namespace != "acme" || cm.Data["a.conf"] != "x" {
		t.Fatalf("bad cm: %+v", cm)
	}
	// Same-name PUT from another tenant is refused.
	if _, err := c.PutConfigMap(ctx, "cfg", map[string]string{"a.conf": "y"}, "bob", ""); err == nil {
		t.Fatal("cross-tenant put must fail")
	}
	all, err := c.ListConfigMaps(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %v %v", all, err)
	}
	// A1: an UNOWNED object (creator "") is not overwritable by anyone.
	if _, err := c.PutConfigMap(ctx, "unowned", map[string]string{"k": "v"}, "", ""); err != nil {
		t.Fatalf("put unowned: %v", err)
	}
	if _, err := c.PutConfigMap(ctx, "unowned", map[string]string{"k": "v2"}, "bob", ""); err == nil {
		t.Fatal("A1: overwriting an unowned configmap must fail")
	}
	ok, err := c.DeleteConfigMap(ctx, "cfg")
	if err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}

func TestSecretPutGetListDelete(t *testing.T) {
	c := newTestClient()
	ctx := context.Background()
	sec, err := c.PutSecret(ctx, "sec", map[string]string{"tok": "s3cr3t"}, "alice", "acme")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if sec.Data["tok"] != "s3cr3t" {
		t.Fatalf("bad secret: %+v", sec)
	}
	if _, err := c.PutSecret(ctx, "sec", map[string]string{"tok": "x"}, "bob", ""); err == nil {
		t.Fatal("cross-tenant put must fail")
	}
	all, err := c.ListSecrets(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list: %v %v", all, err)
	}
}
