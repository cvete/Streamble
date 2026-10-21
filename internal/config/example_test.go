package config

import "testing"

// The example config in the repository root must stay valid.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../streamble.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Apps) == 0 || len(c.Transcode.Profiles) == 0 || len(c.Webhooks) == 0 {
		t.Fatalf("example lost sections: %d apps, %d profiles, %d webhooks", len(c.Apps), len(c.Transcode.Profiles), len(c.Webhooks))
	}
	if c.Storage.Driver != "sqlite" || c.Stats.RetentionDays != 30 {
		t.Fatalf("storage %+v stats %+v", c.Storage, c.Stats)
	}
}
