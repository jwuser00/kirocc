package config

import "testing"

func TestApplyString(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		setEnv   bool
		initial  string
		expected string
	}{
		{"set", "hello", true, "default", "hello"},
		{"unset", "", false, "default", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv("TEST_VAR", tt.envVal)
			}
			s := tt.initial
			applyString("TEST_VAR", &s)
			if s != tt.expected {
				t.Fatalf("got %q, want %q", s, tt.expected)
			}
		})
	}
}

func TestApplyInt(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		initial  int
		expected int
		wantErr  bool
	}{
		{"valid", "9999", 3456, 9999, false},
		{"invalid", "notanumber", 3456, 3456, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TEST_PORT", tt.envVal)
			n := tt.initial
			err := applyInt("TEST_PORT", &n)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if n != tt.expected {
				t.Fatalf("got %d, want %d", n, tt.expected)
			}
		})
	}
}

func TestApplyBool(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		setEnv   bool
		initial  bool
		expected bool
		wantErr  bool
	}{
		{"1", "1", true, false, true, false},
		{"true", "true", true, false, true, false},
		{"false", "false", true, true, false, false},
		{"0", "0", true, true, false, false},
		{"invalid", "notabool", true, false, false, true},
		{"unset", "", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv("TEST_DEBUG", tt.envVal)
			}
			b := tt.initial
			err := applyBool("TEST_DEBUG", &b)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if b != tt.expected {
				t.Fatalf("got %v, want %v", b, tt.expected)
			}
		})
	}
}

func TestDefaultDBPathFor(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		want string
	}{
		{
			name: "darwin",
			goos: "darwin",
			home: "/Users/dkuro",
			want: "/Users/dkuro/Library/Application Support/kiro-cli/data.sqlite3",
		},
		{
			name: "linux",
			goos: "linux",
			home: "/home/dkuro",
			want: "/home/dkuro/.local/share/kiro-cli/data.sqlite3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultDBPathFor(tt.goos, tt.home); got != tt.want {
				t.Fatalf("DefaultDBPathFor(%q, %q) = %q, want %q", tt.goos, tt.home, got, tt.want)
			}
		})
	}
}

func TestApplyEnvOverrides_LogFields(t *testing.T) {
	t.Setenv("KIROCC_LOG_FILE", "/tmp/test.log")
	t.Setenv("KIROCC_LOG_MAX_SIZE", "50")
	t.Setenv("KIROCC_LOG_MAX_BACKUPS", "10")
	t.Setenv("KIROCC_LOG_MAX_AGE", "30")
	t.Setenv("KIROCC_LOG_COMPRESS", "true")
	t.Setenv("KIROCC_LOG_CONSOLE", "true")

	cfg := Config{}
	if err := ApplyEnvOverrides(&cfg); err != nil {
		t.Fatalf("ApplyEnvOverrides: %v", err)
	}
	if cfg.LogFile.Path != "/tmp/test.log" {
		t.Errorf("LogFile.Path = %q, want %q", cfg.LogFile.Path, "/tmp/test.log")
	}
	if cfg.LogFile.MaxSize != 50 {
		t.Errorf("LogFile.MaxSize = %d, want 50", cfg.LogFile.MaxSize)
	}
	if cfg.LogFile.MaxBackups != 10 {
		t.Errorf("LogFile.MaxBackups = %d, want 10", cfg.LogFile.MaxBackups)
	}
	if cfg.LogFile.MaxAge != 30 {
		t.Errorf("LogFile.MaxAge = %d, want 30", cfg.LogFile.MaxAge)
	}
	if !cfg.LogFile.Compress {
		t.Error("LogFile.Compress = false, want true")
	}
	if !cfg.LogFile.Console {
		t.Error("LogFile.Console = false, want true")
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"valid defaults", Config{Host: "127.0.0.1", Port: 3456}, false},
		{"empty host", Config{Port: 3456}, true},
		{"port zero", Config{Host: "127.0.0.1", Port: 0}, true},
		{"port negative", Config{Host: "127.0.0.1", Port: -1}, true},
		{"port too large", Config{Host: "127.0.0.1", Port: 70000}, true},
		{"negative otel body limit", Config{Host: "127.0.0.1", Port: 3456, OTelBodyLimit: -1}, true},
		{"empty base url is allowed", Config{Host: "127.0.0.1", Port: 3456, BaseURL: ""}, false},
		{"valid https base url", Config{Host: "127.0.0.1", Port: 3456, BaseURL: "https://runtime.us-east-1.kiro.dev/"}, false},
		{"valid http base url", Config{Host: "127.0.0.1", Port: 3456, BaseURL: "http://localhost:8080/"}, false},
		{"base url without scheme", Config{Host: "127.0.0.1", Port: 3456, BaseURL: "runtime.us-east-1.kiro.dev"}, true},
		{"base url with unsupported scheme", Config{Host: "127.0.0.1", Port: 3456, BaseURL: "ftp://example.com/"}, true},
		{"base url without host", Config{Host: "127.0.0.1", Port: 3456, BaseURL: "https:///path"}, true},
		{"max body size zero means unlimited", Config{Host: "127.0.0.1", Port: 3456, MaxBodySize: 0}, false},
		{"max body size positive", Config{Host: "127.0.0.1", Port: 3456, MaxBodySize: 1 << 20}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyEnvOverrides_RegionAndBaseURL(t *testing.T) {
	t.Setenv("KIROCC_REGION", "us-east-1")
	t.Setenv("KIROCC_BASE_URL", "http://localhost:9999/")

	cfg := Config{}
	if err := ApplyEnvOverrides(&cfg); err != nil {
		t.Fatalf("ApplyEnvOverrides: %v", err)
	}
	if cfg.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", cfg.Region, "us-east-1")
	}
	if cfg.BaseURL != "http://localhost:9999/" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "http://localhost:9999/")
	}
}

func TestApplyEnvOverrides_RegionUnsetKeepsFlagValue(t *testing.T) {
	t.Setenv("KIROCC_REGION", "")

	cfg := Config{Region: "eu-central-1"}
	if err := ApplyEnvOverrides(&cfg); err != nil {
		t.Fatalf("ApplyEnvOverrides: %v", err)
	}
	if cfg.Region != "eu-central-1" {
		t.Errorf("Region = %q, want flag value %q to survive", cfg.Region, "eu-central-1")
	}
}

func TestApplyEnvOverrides_MaxBodySize(t *testing.T) {
	t.Setenv("KIROCC_MAX_BODY_SIZE", "67108864")

	cfg := Config{MaxBodySize: 1 << 20}
	if err := ApplyEnvOverrides(&cfg); err != nil {
		t.Fatalf("ApplyEnvOverrides: %v", err)
	}
	if cfg.MaxBodySize != 67108864 {
		t.Errorf("MaxBodySize = %d, want %d", cfg.MaxBodySize, 67108864)
	}
}

func TestApplyEnvOverrides_MaxBodySizeInvalid(t *testing.T) {
	t.Setenv("KIROCC_MAX_BODY_SIZE", "not-a-number")

	cfg := Config{}
	if err := ApplyEnvOverrides(&cfg); err == nil {
		t.Error("expected an error for a non-numeric KIROCC_MAX_BODY_SIZE")
	}
}
