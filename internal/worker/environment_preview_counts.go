package worker

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// copyCounter counts the rows of every table in pg_restore's plain-text data output. COPY text
// format escapes newlines inside values, so each line of a COPY block is exactly one row.
type copyCounter struct {
	partial []byte
	table   string
	inCopy  bool
	totals  map[string]int64
}

func newCopyCounter() *copyCounter { return &copyCounter{totals: map[string]int64{}} }

func (c *copyCounter) Write(p []byte) (int, error) {
	n := len(p)
	data := append(c.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		c.line(data[:i])
		data = data[i+1:]
	}
	c.partial = append(c.partial[:0], data...)
	return n, nil
}

func (c *copyCounter) line(line []byte) {
	if c.inCopy {
		if string(line) == `\.` {
			c.inCopy = false
			return
		}
		c.totals[c.table]++
		return
	}
	if !bytes.HasPrefix(line, []byte("COPY ")) || !bytes.HasSuffix(line, []byte("FROM stdin;")) {
		return
	}
	name, ok := parseQualifiedName(string(line[5:]))
	if !ok {
		return
	}
	c.table, c.inCopy = name, true
	c.totals[name] += 0
}

func (c *copyCounter) counts() map[string]int64 { return c.totals }

// parseQualifiedName reads schema.table from the start of s, accepting quoted identifiers.
func parseQualifiedName(s string) (string, bool) {
	parts := []string{}
	for len(parts) < 2 {
		var part string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s); i++ {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i++
						continue
					}
					break
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return "", false
			}
			part, s = b.String(), s[i+1:]
		} else {
			end := strings.IndexAny(s, ". (")
			if end <= 0 {
				return "", false
			}
			part, s = s[:end], s[end:]
		}
		parts = append(parts, part)
		if !strings.HasPrefix(s, ".") {
			break
		}
		s = s[1:]
	}
	if len(parts) != 2 {
		return "", false
	}
	return parts[0] + "." + parts[1], true
}

// countMariaDBRows counts rows per table in a mariadb-dump file by counting the value tuples of
// each INSERT statement, which ends at the first semicolon outside a quoted value. Tables created
// without rows are reported with zero rows.
func countMariaDBRows(r io.Reader, database string) (map[string]int64, error) {
	reader := bufio.NewReaderSize(r, 1<<20)
	totals := map[string]int64{}
	var prefix []byte
	atLineStart := true
	inInsert := false
	table := ""
	depth, quote := 0, byte(0)
	escaped := false
	for {
		b, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if inInsert {
			switch {
			case escaped:
				escaped = false
			case quote != 0:
				if b == '\\' {
					escaped = true
				} else if b == quote {
					quote = 0
				}
			case b == '\'' || b == '"':
				quote = b
			case b == '(':
				if depth == 0 {
					totals[table]++
				}
				depth++
			case b == ')':
				depth--
			case b == ';' && depth == 0:
				inInsert = false
				atLineStart = false
				prefix = prefix[:0]
			}
			continue
		}
		if b == '\n' {
			atLineStart = true
			prefix = prefix[:0]
			continue
		}
		if !atLineStart {
			continue
		}
		prefix = append(prefix, b)
		line := string(prefix)
		if name, ok := backquotedAfter(line, "INSERT INTO "); ok {
			// MariaDB 11 puts each tuple on its own line after VALUES; older versions continue the line.
			if strings.HasSuffix(line, " VALUES") {
				table = database + "." + name
				inInsert, depth, quote, escaped = true, 0, 0, false
				totals[table] += 0
			}
		} else if name, ok := backquotedAfter(line, "CREATE TABLE "); ok && strings.HasSuffix(line, "` (") {
			totals[database+"."+name] += 0
			atLineStart = false
		}
		if len(prefix) > 512 {
			atLineStart = false
		}
	}
	return totals, nil
}

func backquotedAfter(line, keyword string) (string, bool) {
	if !strings.HasPrefix(line, keyword+"`") {
		return "", false
	}
	rest := line[len(keyword)+1:]
	end := strings.IndexByte(rest, '`')
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}
