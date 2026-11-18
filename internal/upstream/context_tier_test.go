package upstream

import "testing"

func TestConfigureContextTier(t *testing.T) {
	always := func(string) bool { return true }

	t.Run("auto", func(t *testing.T) {
		c := &Client{}
		c.ConfigureContextTier("", func(id string) bool { return id == "big" })
		if c.DefaultContextTier != "long_context" || c.LongContextChecker == nil {
			t.Fatalf("auto mode misconfigured: %+v", c)
		}
		if !c.LongContextChecker("big") || c.LongContextChecker("small") {
			t.Fatal("checker not wired through")
		}
	})
	t.Run("off", func(t *testing.T) {
		c := &Client{DefaultContextTier: "x", LongContextChecker: always}
		c.ConfigureContextTier("off", always)
		if c.DefaultContextTier != "" || c.LongContextChecker != nil {
			t.Fatalf("off mode must clear injection: %+v", c)
		}
	})
	t.Run("force", func(t *testing.T) {
		c := &Client{}
		c.ConfigureContextTier(" custom ", always)
		if c.DefaultContextTier != "custom" || c.LongContextChecker != nil {
			t.Fatalf("force mode misconfigured: %+v", c)
		}
	})
}
