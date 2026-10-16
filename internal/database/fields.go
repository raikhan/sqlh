package database

import (
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/raikhan/sqlh/dialect"
)

// Field is a labelled part of a connection, for display (passwords masked).
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

const masked = "••••••"

// dsnParts: the parts of a connection string, by our labels.
type dsnParts struct {
	host, port, db, user, password, path, sslmode, searchPath string
	params                                                    [][2]string
}

// parseDSN reads the usual connection string forms: PostgreSQL key=value,
// URLs (postgres://, oracle://, sqlserver://, …), MySQL user:pw@tcp(h:p)/db,
// SQLite paths and file: URIs.
func parseDSN(driver dialect.DatabaseDriver, dsn string) dsnParts {
	var d dsnParts
	dsn = strings.TrimSpace(dsn)
	switch {
	case driver == dialect.DatabaseDriverSQLite3:
		path := strings.TrimPrefix(dsn, "file:")
		if i := strings.IndexByte(path, '?'); i >= 0 {
			for _, kv := range strings.Split(path[i+1:], "&") {
				k, v, _ := strings.Cut(kv, "=")
				d.params = append(d.params, [2]string{k, v})
			}
			path = path[:i]
		}
		d.path = path
	case strings.Contains(dsn, "://"):
		u, err := url.Parse(dsn)
		if err != nil {
			return d
		}
		d.host, d.port = u.Hostname(), u.Port()
		if u.User != nil {
			d.user = u.User.Username()
			if _, ok := u.User.Password(); ok {
				d.password = masked
			}
		}
		d.db = strings.TrimPrefix(u.Path, "/")
		for k, vs := range u.Query() {
			v := strings.Join(vs, ",")
			switch strings.ToLower(k) {
			case "database", "dbname":
				d.db = v
			case "sslmode":
				d.sslmode = v
			case "search_path":
				d.searchPath = v
			default:
				d.params = append(d.params, [2]string{k, v})
			}
		}
	case driver == dialect.DatabaseDriverMySQL || strings.Contains(dsn, "@tcp(") || strings.Contains(dsn, "@unix("):
		at := strings.LastIndex(dsn, "@")
		if at >= 0 {
			cred := dsn[:at]
			user, pw, hasPW := strings.Cut(cred, ":")
			d.user = user
			if hasPW && pw != "" {
				d.password = masked
			}
			dsn = dsn[at+1:]
		}
		if open := strings.IndexByte(dsn, '('); open >= 0 {
			if close := strings.IndexByte(dsn, ')'); close > open {
				addr := dsn[open+1 : close]
				if h, p, ok := strings.Cut(addr, ":"); ok && !strings.HasPrefix(dsn, "unix") {
					d.host, d.port = h, p
				} else {
					d.path = addr
				}
				dsn = dsn[close+1:]
			}
		}
		dsn = strings.TrimPrefix(dsn, "/")
		db, q, _ := strings.Cut(dsn, "?")
		d.db = db
		for _, kv := range strings.Split(q, "&") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				d.params = append(d.params, [2]string{k, v})
			}
		}
	default: // key=value (PostgreSQL)
		for _, kv := range keyValues(dsn) {
			k, v := strings.ToLower(kv[0]), kv[1]
			switch k {
			case "host":
				d.host = v
			case "port":
				d.port = v
			case "dbname", "database":
				d.db = v
			case "user":
				d.user = v
			case "password":
				d.password = masked
			case "sslmode":
				d.sslmode = v
			case "options":
				if m := searchPathRe.FindStringSubmatch(v); m != nil {
					d.searchPath = m[1]
				}
				d.params = append(d.params, [2]string{k, v})
			default:
				d.params = append(d.params, [2]string{k, v})
			}
		}
	}
	return d
}

var searchPathRe = regexp.MustCompile(`search_path\s*=\s*([^\s']+)`)

// keyValues splits `k=v k='v w'` pairs.
func keyValues(s string) [][2]string {
	var out [][2]string
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t\n")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		k := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " ")
		var v string
		if strings.HasPrefix(s, "'") {
			end := 1
			var b strings.Builder
			for end < len(s) {
				if s[end] == '\\' && end+1 < len(s) {
					b.WriteByte(s[end+1])
					end += 2
					continue
				}
				if s[end] == '\'' {
					break
				}
				b.WriteByte(s[end])
				end++
			}
			v = b.String()
			s = s[min(end+1, len(s)):]
		} else {
			end := strings.IndexAny(s, " \t\n")
			if end < 0 {
				end = len(s)
			}
			v, s = s[:end], s[end:]
		}
		out = append(out, [2]string{k, v})
	}
	return out
}

// Target is the database and schema a connection works in ("" = unknown:
// the login's default).
func (c *DBConfig) Target() (db, schema string) {
	d := parseDSN(c.Driver, c.DataSourceName)
	db = c.DBName
	if db == "" {
		db = d.db
	}
	if c.Driver == dialect.DatabaseDriverSQLite3 {
		p := c.Path
		if p == "" {
			p = d.path
		}
		if p == "" {
			p = c.DataSourceName
		}
		return filepath.Base(p), ""
	}
	schema = c.CurrentSchema()
	if schema == "" && d.searchPath != "" {
		schema = strings.TrimSpace(strings.Split(d.searchPath, ",")[0])
	}
	if schema == "" && c.Driver == dialect.DatabaseDriverOracle {
		user := c.User
		if user == "" {
			user = d.user
		}
		schema = strings.ToUpper(user)
	}
	return db, schema
}

// Fields describes a connection part by part (passwords masked): what the
// panel shows. Connection strings are taken apart.
func (c *DBConfig) Fields() []Field {
	var out []Field
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			out = append(out, Field{label, value})
		}
	}
	d := parseDSN(c.Driver, c.DataSourceName)
	pick := func(own, fromDSN string) string {
		if own != "" {
			return own
		}
		return fromDSN
	}
	add("Driver", string(c.Driver))
	if c.Proto != "" {
		add("Protocol", string(c.Proto))
	}
	port := d.port
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	if c.Driver == dialect.DatabaseDriverSQLite3 {
		add("File", pick(c.Path, d.path))
	} else {
		add("Host", pick(c.Host, d.host))
		add("Port", port)
		add("Socket", pick(c.Path, d.path))
	}
	db, schema := c.Target()
	if c.Driver != dialect.DatabaseDriverSQLite3 {
		add("Database", db)
		add("Schema", schema)
	}
	add("User", pick(c.User, d.user))
	switch {
	case c.Passwd != "":
		add("Password", masked)
	case c.PasswdCmd != "":
		add("Password cmd", c.PasswdCmd)
	default:
		add("Password", d.password)
	}
	add("SSL mode", pick(c.Params["sslmode"], d.sslmode))
	for _, kv := range d.params {
		add(kv[0], kv[1])
	}
	keys := make([]string, 0, len(c.Params))
	for k := range c.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != "sslmode" {
			add(k, c.Params[k])
		}
	}
	if len(c.Schemas) > 0 {
		add("Schemas", strings.Join(c.Schemas, ", "))
	}
	if c.SSHCfg != nil {
		add("SSH", c.SSHCfg.User+"@"+c.SSHCfg.Host+":"+strconv.Itoa(c.SSHCfg.Port))
	}
	return out
}
