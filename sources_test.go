package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceSettingsFlow(t *testing.T) {
	r := testReceiver(t)
	save := func(body string) int {
		req := httptest.NewRequest("POST", "/api/sources", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.setSources(w, req)
		return w.Code
	}
	if code := save(`{"source_mode":"direct","website_sources":[{"directory":"/www/backup/site/../etc"}]}`); code != 400 {
		t.Fatalf("unsafe source accepted: %d", code)
	}
	body := `{"source_mode":"direct","website_sources":[{"directory":"/www/backup/site/web"}],"database_sources":[{"directory":"/www/backup/database/mysql","pattern":"backup-{date}*.sql.gz"}]}`
	if code := save(body); code != 200 {
		t.Fatalf("save failed: %d", code)
	}
	if r.sources.Revision != 1 || r.sources.ApplyStatus != "pending" || r.sources.Websites[0].Pattern != defaultSitePattern {
		t.Fatalf("unexpected saved sources: %+v", r.sources)
	}
	reply, err := r.execute("bz-config", command{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var wire result
	if err := json.Unmarshal(raw, &wire); err != nil || wire.SourceSelection == nil || wire.Mode != "direct" || wire.Revision != 1 {
		t.Fatalf("invalid SSH result: %s, %v", raw, err)
	}
	bad, good := false, true
	if _, err := r.execute("bz-config-result", command{Revision: 1, ConfigOK: &bad, Error: "dir unavailable"}); err != nil {
		t.Fatal(err)
	}
	if r.sources.ApplyStatus != "failed" || r.sources.ApplyError != "dir unavailable" {
		t.Fatalf("unexpected failure result: %+v", r.sources)
	}
	if _, err := r.execute("bz-config-result", command{Revision: 0, ConfigOK: &good}); err != nil || r.sources.ApplyStatus != "failed" {
		t.Fatal("obsolete report changed the current settings")
	}
	if _, err := r.execute("bz-config-result", command{Revision: 1, ConfigOK: &good}); err != nil {
		t.Fatal(err)
	}
	r, err = openReceiver(r.stateDir, r.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if r.sources.AppliedRevision != 1 || r.sources.ApplyStatus != "applied" || r.sources.CheckedAt == nil {
		t.Fatalf("application result was not persisted: %+v", r.sources)
	}
	if code := save(body); code != 200 || r.sources.Revision != 1 {
		t.Fatal("identical config created a new revision")
	}
}
