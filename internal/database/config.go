package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/raikhan/sqlh/dialect"
	"golang.org/x/crypto/ssh"
)

type Proto string

const (
	ProtoTCP  Proto = "tcp"
	ProtoUDP  Proto = "udp"
	ProtoUnix Proto = "unix"
	ProtoHTTP Proto = "http"
)

type DBConfig struct {
	Alias          string                 `json:"alias" yaml:"alias"`
	Driver         dialect.DatabaseDriver `json:"driver" yaml:"driver"`
	DataSourceName string                 `json:"dataSourceName" yaml:"dataSourceName"`
	Proto          Proto                  `json:"proto" yaml:"proto"`
	User           string                 `json:"user" yaml:"user"`
	Passwd         string                 `json:"passwd" yaml:"passwd"`
	// PasswdCmd is a shell command whose trimmed stdout is the password, e.g.
	// "security find-generic-password -s cadev1 -w". Used when Passwd is empty.
	PasswdCmd string            `json:"passwdCmd" yaml:"passwdCmd"`
	Host      string            `json:"host" yaml:"host"`
	Port      int               `json:"port" yaml:"port"`
	Path      string            `json:"path" yaml:"path"`
	DBName    string            `json:"dbName" yaml:"dbName"`
	Params    map[string]string `json:"params" yaml:"params"`
	SSHCfg    *SSHConfig        `json:"sshConfig" yaml:"sshConfig"`
	// Schemas is the metadata allow-list used for completion/hover. Oracle:
	// empty means the login schema only. PostgreSQL: empty means all schemas.
	Schemas []string `json:"schemas" yaml:"schemas"`
}

// envRefRe matches ${NAME}. Bare $NAME is deliberately not expanded so that
// literal '$' in existing passwords/DSNs keeps working.
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ResolveSecrets returns a copy of the config with ${ENV} references expanded
// in user, passwd and dataSourceName, and passwdCmd executed when passwd is
// empty. It runs at connect time, so a keychain prompt only appears for the
// connection actually used.
func (c *DBConfig) ResolveSecrets() (*DBConfig, error) {
	r := *c
	expand := func(v string) (string, error) {
		var missing []string
		out := envRefRe.ReplaceAllStringFunc(v, func(ref string) string {
			name := envRefRe.FindStringSubmatch(ref)[1]
			val, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return "", fmt.Errorf("connection %q: environment variable(s) not set: %s", c.Alias, strings.Join(missing, ", "))
		}
		return out, nil
	}
	var err error
	if r.User, err = expand(r.User); err != nil {
		return nil, err
	}
	if r.Passwd, err = expand(r.Passwd); err != nil {
		return nil, err
	}
	if r.DataSourceName, err = expand(r.DataSourceName); err != nil {
		return nil, err
	}
	if r.Passwd == "" && r.PasswdCmd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "sh", "-c", r.PasswdCmd).Output()
		if err != nil {
			msg := err.Error()
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				msg = strings.TrimSpace(string(ee.Stderr))
			}
			return nil, fmt.Errorf("connection %q: passwdCmd failed: %s", c.Alias, msg)
		}
		r.Passwd = strings.TrimRight(string(out), "\r\n")
	}
	return &r, nil
}

func (c *DBConfig) Validate() error {
	if c.Driver == "" {
		return errors.New("required: connections[].driver")
	}

	switch c.Driver {
	case
		dialect.DatabaseDriverMySQL,
		dialect.DatabaseDriverMySQL8,
		dialect.DatabaseDriverMySQL57,
		dialect.DatabaseDriverMySQL56,
		dialect.DatabaseDriverPostgreSQL,
		dialect.DatabaseDriverVertica:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}

		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP, ProtoUDP, ProtoHTTP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUnix:
				if c.Path == "" {
					return errors.New("required: connections[].path")
				}
			default:
				return errors.New("invalid: connections[].proto")
			}
			if c.SSHCfg != nil {
				return c.SSHCfg.Validate()
			}
		}
	case dialect.DatabaseDriverSQLite3:
	case dialect.DatabaseDriverH2:
		if c.DataSourceName == "" {
			return errors.New("required: connections[].dataSourceName")
		}
	case dialect.DatabaseDriverMssql:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}
		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUDP, ProtoUnix, ProtoHTTP:
			default:
				return errors.New("invalid: connections[].proto")
			}
		}
	case dialect.DatabaseDriverOracle:
		// dataSourceName (a go-ora URL) or host + dbName (service) + user +
		// passwd/passwdCmd. proto is not used; port defaults to 1521.
		if c.DataSourceName == "" {
			if c.Host == "" {
				return errors.New("required: connections[].dataSourceName or connections[].host")
			}
			if c.DBName == "" {
				return errors.New("required: connections[].dbName (service name)")
			}
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			if c.Passwd == "" && c.PasswdCmd == "" {
				return errors.New("required: connections[].passwd or connections[].passwdCmd")
			}
		}
	case dialect.DatabaseDriverClickhouse:
		if c.DataSourceName == "" && c.Proto == "" {
			return errors.New("required: connections[].dataSourceName or connections[].proto")
		}

		if c.DataSourceName == "" && c.Proto != "" {
			if c.User == "" {
				return errors.New("required: connections[].user")
			}
			switch c.Proto {
			case ProtoTCP, ProtoHTTP:
				if c.Host == "" {
					return errors.New("required: connections[].host")
				}
			case ProtoUDP, ProtoUnix:
			default:
				return errors.New("invalid: connections[].proto")
			}
			if c.SSHCfg != nil {
				return c.SSHCfg.Validate()
			}
		}

	default:
		return errors.New("invalid: connections[].driver")
	}
	return nil
}

type SSHConfig struct {
	Host       string `json:"host" yaml:"host"`
	Port       int    `json:"port" yaml:"port"`
	User       string `json:"user" yaml:"user"`
	PassPhrase string `json:"passPhrase" yaml:"passPhrase"`
	PrivateKey string `json:"privateKey" yaml:"privateKey"`
}

func (s *SSHConfig) Validate() error {
	if s.Host == "" {
		return errors.New("required: connections[]sshConfig.host")
	}
	if s.User == "" {
		return errors.New("required: connections[].sshConfig.user")
	}
	if s.PrivateKey == "" {
		return errors.New("required: connections[].sshConfig.privateKey")
	}
	return nil
}

func (s *SSHConfig) Endpoint() string {
	port := s.Port
	if port == 0 {
		port = 22
	}
	return fmt.Sprintf("%s:%d", s.Host, port)
}

func (s *SSHConfig) ClientConfig() (*ssh.ClientConfig, error) {
	buffer, err := os.ReadFile(s.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("cannot read SSH private key file, PrivateKey=%s, %w", s.PrivateKey, err)
	}

	var key ssh.Signer
	if s.PassPhrase != "" {
		key, err = ssh.ParsePrivateKeyWithPassphrase(buffer, []byte(s.PassPhrase))
		if err != nil {
			return nil, fmt.Errorf("cannot parse SSH private key file with passphrase, PrivateKey=%s, %w", s.PrivateKey, err)
		}
	} else {
		key, err = ssh.ParsePrivateKey(buffer)
		if err != nil {
			return nil, fmt.Errorf("cannot parse SSH private key file, PrivateKey=%s, %w", s.PrivateKey, err)
		}
	}

	sshConfig := &ssh.ClientConfig{
		User:            s.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	return sshConfig, nil
}
