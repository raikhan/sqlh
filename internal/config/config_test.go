package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/raikhan/sqlh/internal/database"
)

func TestGetConfig(t *testing.T) {
	type args struct {
		fp string
	}
	tests := []struct {
		name    string
		args    args
		want    *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "basic",
			args: args{
				fp: "basic.yml",
			},
			want: &Config{
				LowercaseKeywords: true,
				Connections: []*database.DBConfig{
					{
						Alias:  "sqls_mysql",
						Driver: "mysql",
						Proto:  "tcp",
						User:   "root",
						Passwd: "root",
						Host:   "127.0.0.1",
						Port:   13306,
						DBName: "world",
						Params: map[string]string{"autocommit": "true", "tls": "skip-verify"},
					},
					{
						Alias:          "sqls_sqlite3",
						Driver:         "sqlite3",
						DataSourceName: "file:/home/sqlh/chinook.db",
					},
					{
						Alias:  "sqls_postgresql",
						Driver: "postgresql",
						Proto:  "tcp",
						User:   "postgres",
						Passwd: "mysecretpassword1234",
						Host:   "127.0.0.1",
						Port:   15432,
						DBName: "dvdrental",
						Params: map[string]string{"sslmode": "disable"},
					},
					{
						Alias:  "mysql_with_bastion",
						Driver: "mysql",
						Proto:  "tcp",
						User:   "admin",
						Passwd: "Q+ACgv12ABx/",
						Host:   "192.168.121.163",
						Port:   3306,
						DBName: "world",
						SSHCfg: &database.SSHConfig{
							Host:       "192.168.121.168",
							Port:       22,
							User:       "vagrant",
							PassPhrase: "passphrase1234",
							PrivateKey: "/home/sqlh/.ssh/id_rsa",
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "no driver",
			args: args{
				fp: "no_driver.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection sqls_mysql: required: connections[].driver",
		},
		{
			name: "no connection",
			args: args{
				fp: "no_connection.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection sqls_mysql: required: connections[].dataSourceName or connections[].proto",
		},
		{
			name: "no user",
			args: args{
				fp: "no_user.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection sqls_mysql: required: connections[].user",
		},
		{
			name: "invalid proto",
			args: args{
				fp: "invalid_proto.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection sqls_mysql: invalid: connections[].proto",
		},
		{
			name: "no path",
			args: args{
				fp: "no_path.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection sqls_mysql: required: connections[].path",
		},
		{
			name: "no dsn",
			args: args{
				fp: "no_dsn.yml",
			},
			want: &Config{
				Connections: []*database.DBConfig{
					{
						Alias:          "sqls_sqlite3",
						Driver:         "sqlite3",
						DataSourceName: "",
					},
				},
			},
			wantErr: true,
			errMsg:  "failed validation, required: connections[].dataSourceName",
		},
		{
			name: "no ssh host",
			args: args{
				fp: "no_ssh_host.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection mysql_with_bastion: required: connections[]sshConfig.host",
		},
		{
			name: "no ssh user",
			args: args{
				fp: "no_ssh_user.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection mysql_with_bastion: required: connections[].sshConfig.user",
		},
		{
			name: "no ssh private key",
			args: args{
				fp: "no_ssh_private_key.yml",
			},
			want:    nil,
			wantErr: true,
			errMsg:  "failed validation, connection mysql_with_bastion: required: connections[].sshConfig.privateKey",
		},
		{
			name: "oracle config",
			args: args{
				fp: "oracle.yaml",
			},
			want: &Config{
				Connections: []*database.DBConfig{
					{
						Alias:          "TestDB",
						Driver:         "oracle",
						DataSourceName: "SYSTEM/P1ssword@localhost:1521/XE",
					},
				},
			},
			wantErr: true,
			errMsg:  "failed validation, connection mysql_with_bastion: required: connections[].sshConfig.privateKey",
		},
	}
	for _, tt := range tests {
		packageDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("cannot get package path, Err=%v", err)
		}
		testFile := filepath.Join(packageDir, "testdata", tt.args.fp)

		t.Run(tt.name, func(t *testing.T) {
			got, err := GetConfig(testFile)
			if err != nil {
				if tt.wantErr {
					if err.Error() != tt.errMsg {
						t.Errorf("unmatch error message, want:%q got:%q", tt.errMsg, err.Error())
					}
				} else {
					t.Errorf("GetConfig() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unmatch (- want, + got):\n%s", diff)
			}
		})
	}
}

func TestReuseWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"":    DefaultReuseFor,
		"10m": 10 * time.Minute,
		"90s": 90 * time.Second,
		"1h":  time.Hour,
		"0":   0,
		"30":  30 * time.Second,
	}
	for in, want := range cases {
		if got := (ResultsConfig{ReuseFor: in}).ReuseWindow(); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	for _, bad := range []string{"soon", "-5m"} {
		c := &Config{Results: ResultsConfig{ReuseFor: bad}}
		if err := c.Validate(); err == nil {
			t.Errorf("%q: want a validation error", bad)
		}
	}
	// YAML: a bare 0 or number decodes into the string field
	for src, want := range map[string]time.Duration{"results:\n  reuseFor: 0\n": 0, "results:\n  reuseFor: 5m\n": 5 * time.Minute} {
		p := filepath.Join(t.TempDir(), "c.yml")
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := GetConfig(p)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if got := cfg.Results.ReuseWindow(); got != want {
			t.Errorf("%q: got %s want %s", src, got, want)
		}
	}
}
