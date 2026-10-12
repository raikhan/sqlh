package database

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var dsnPasswordRe = regexp.MustCompile(`(?i)(password=)('[^']*'|\S+)|(://[^:/@]+:)([^@]+)(@)`)

// DescribeConnection describes a connection without exposing passwords.
func DescribeConnection(conn *DBConfig) string {
	if conn.DataSourceName != "" {
		return dsnPasswordRe.ReplaceAllString(conn.DataSourceName, "${1}${3}***${5}")
	}
	var desc string
	switch conn.Proto {
	case ProtoTCP:
		desc = fmt.Sprintf("tcp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	case ProtoUDP:
		desc = fmt.Sprintf("udp(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	case ProtoUnix:
		desc = fmt.Sprintf("unix(%s)/%s", conn.Path, conn.DBName)
	case ProtoHTTP:
		desc = fmt.Sprintf("http(%s:%d)/%s", conn.Host, conn.Port, conn.DBName)
	default:
		switch {
		case conn.Path != "":
			desc = conn.Path
		case conn.Host != "":
			desc = conn.Host
			if conn.Port != 0 {
				desc += ":" + strconv.Itoa(conn.Port)
			}
			desc += "/" + conn.DBName
		default:
			desc = conn.DBName
		}
	}
	if conn.User != "" && desc != "" {
		desc = conn.User + "@" + desc
	}
	if len(conn.Schemas) > 0 {
		desc += " schemas=" + strings.Join(conn.Schemas, ",")
	}
	return desc
}
