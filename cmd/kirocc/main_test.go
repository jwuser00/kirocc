package main

import (
	"context"
	"testing"
)

func TestRun_HelpFlagReturnsNoError(t *testing.T) {
	if err := run(context.Background(), []string{"-h"}); err != nil {
		t.Errorf("run with -h: got err %v; want nil", err)
	}
}

func TestParseFlags_Region(t *testing.T) {
	t.Run("default is empty so the credential region is used", func(t *testing.T) {
		cfg, err := parseFlags(nil)
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if cfg.Region != "" {
			t.Fatalf("Region = %q, want empty", cfg.Region)
		}
	})

	for _, flagName := range []string{"-region", "-kiro-api-region"} {
		t.Run(flagName, func(t *testing.T) {
			cfg, err := parseFlags([]string{flagName, "us-east-1"})
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if cfg.Region != "us-east-1" {
				t.Fatalf("Region = %q, want us-east-1", cfg.Region)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}

	t.Run("invalid region is rejected by Validate", func(t *testing.T) {
		cfg, err := parseFlags([]string{"-kiro-api-region", "evil.example.com"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("Validate: want error for a non-region value")
		}
	})
}

func TestParseFlags_ModelDiscovery(t *testing.T) {
	t.Run("default enabled", func(t *testing.T) {
		cfg, err := parseFlags(nil)
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if !cfg.ModelDiscovery {
			t.Fatal("ModelDiscovery = false, want true")
		}
	})

	t.Run("disabled by flag", func(t *testing.T) {
		cfg, err := parseFlags([]string{"-model-discovery=false"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if cfg.ModelDiscovery {
			t.Fatal("ModelDiscovery = true, want false")
		}
	})
}
