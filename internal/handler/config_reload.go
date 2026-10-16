package handler

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/raikhan/sqlh/internal/catalog"
	"github.com/raikhan/sqlh/internal/config"
	"github.com/raikhan/sqlh/internal/control"
	"github.com/raikhan/sqlh/internal/database"
)

// editableConfigPath: the file the server's config comes from ("" when it
// comes from the editor's initializationOptions).
func (s *Server) editableConfigPath() string {
	if s.SpecificFileCfg == nil && s.WSCfg != nil {
		return ""
	}
	return s.ConfigPath
}

// NoteConfigRead records the config file's modification time (call once
// the config is loaded).
func (s *Server) NoteConfigRead() {
	if p := s.editableConfigPath(); p != "" {
		if fi, err := os.Stat(p); err == nil {
			s.configMod = fi.ModTime()
		}
	}
}

// reloadConfigIfChanged re-reads the config file when it changed since it
// was read. The current connection follows its alias (reconnecting when
// its settings changed). s.mu must be held.
func (s *Server) reloadConfigIfChanged(ctx context.Context) (string, error) {
	path := s.editableConfigPath()
	if path == "" {
		return "the config comes from the editor", nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !s.configMod.IsZero() && fi.ModTime().Equal(s.configMod) {
		return "unchanged", nil
	}
	cfg, err := config.GetConfig(path)
	if err != nil {
		log.Println("config reload:", err)
		return "", err
	}
	s.configMod = fi.ModTime()
	old := s.curDBCfg
	if s.SpecificFileCfg != nil {
		s.SpecificFileCfg = cfg
	} else {
		s.DefaultFileCfg = cfg
	}
	msg := "config reloaded"
	if old != nil && old.Alias != "" {
		for i, c := range cfg.Connections {
			if c.Alias != old.Alias {
				continue
			}
			s.curConnectionIndex = i
			if !reflect.DeepEqual(*c, *old) && s.dbConn != nil {
				if m, err := s.switchTo(ctx, c.Alias); err != nil {
					log.Println("config reload: reconnect:", err)
				} else {
					msg = m
				}
			}
			break
		}
	}
	s.emit(control.Event{Kind: control.EventConfig})
	return msg, nil
}

// allColumns lists every column of the schema cache (the panel's search).
func (s *Server) allColumns() ([]control.ColumnRef, error) {
	src, opt, err := s.catalog(false)
	if err != nil {
		return nil, err
	}
	out := []control.ColumnRef{}
	def := src.DefaultSchema()
	for schema, tables := range src.SchemaTables() {
		if catalog.System(opt.Driver, schema) {
			continue
		}
		for _, t := range tables {
			cols, ok := src.Columns(schema, t)
			if !ok {
				continue
			}
			insert := database.QuoteIdent(t, opt.Driver, opt.Lower)
			if schema != "" && !strings.EqualFold(schema, def) {
				insert = database.QuoteIdent(schema, opt.Driver, opt.Lower) + "." + insert
			}
			for _, c := range cols {
				out = append(out, control.ColumnRef{Schema: schema, Table: t, TableInsert: insert, Name: c.Name,
					Insert: database.QuoteIdent(c.Name, opt.Driver, opt.Lower), Type: c.Type})
			}
		}
	}
	return out, nil
}
