// Package store is exo's single SQLite database. Everything the dashboard
// shows comes from here; nothing is cached in memory beyond a query.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  channel TEXT NOT NULL, chat TEXT NOT NULL, sender TEXT NOT NULL, person TEXT NOT NULL,
  guid TEXT UNIQUE, text TEXT NOT NULL DEFAULT '', images TEXT NOT NULL DEFAULT '[]',
  ts INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'new', turn_id INTEGER);
CREATE INDEX IF NOT EXISTS events_pending ON events(person, status);

CREATE TABLE IF NOT EXISTS turns (
  id INTEGER PRIMARY KEY,
  person TEXT NOT NULL, channel TEXT NOT NULL, chat TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'chat',           -- chat | job | notice | replay
  started INTEGER NOT NULL, finished INTEGER,
  engine TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'running',
  input TEXT NOT NULL DEFAULT '', reply TEXT NOT NULL DEFAULT '',
  tools TEXT NOT NULL DEFAULT '',               -- one-line digest of tool use
  latency_ms INTEGER, in_tok INTEGER, out_tok INTEGER, cache_read INTEGER, cache_write INTEGER,
  cost REAL, prompt_hash TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  feedback INTEGER NOT NULL DEFAULT 0, feedback_note TEXT NOT NULL DEFAULT '',
  out_guids TEXT NOT NULL DEFAULT '', attachments TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS turns_person ON turns(person, id);
CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts USING fts5(input, reply, content='turns', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS turns_ai AFTER UPDATE OF reply ON turns WHEN new.reply <> '' BEGIN
  INSERT INTO turns_fts(turns_fts, rowid, input, reply) SELECT 'delete', old.id, old.input, old.reply WHERE old.reply <> '';
  INSERT INTO turns_fts(rowid, input, reply) VALUES (new.id, new.input, new.reply);
END;
CREATE TRIGGER IF NOT EXISTS turns_ins AFTER INSERT ON turns WHEN new.reply <> '' BEGIN
  INSERT INTO turns_fts(rowid, input, reply) VALUES (new.id, new.input, new.reply);
END;

CREATE TABLE IF NOT EXISTS trace (
  turn_id INTEGER NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL, PRIMARY KEY (turn_id, seq));

CREATE TABLE IF NOT EXISTS facts (
  id INTEGER PRIMARY KEY, person TEXT NOT NULL, text TEXT NOT NULL,
  pinned INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(text, content='facts', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS facts_ai AFTER INSERT ON facts BEGIN
  INSERT INTO facts_fts(rowid, text) VALUES (new.id, new.text); END;
CREATE TRIGGER IF NOT EXISTS facts_ad AFTER DELETE ON facts BEGIN
  INSERT INTO facts_fts(facts_fts, rowid, text) VALUES ('delete', old.id, old.text); END;
CREATE TRIGGER IF NOT EXISTS facts_au AFTER UPDATE OF text ON facts BEGIN
  INSERT INTO facts_fts(facts_fts, rowid, text) VALUES ('delete', old.id, old.text);
  INSERT INTO facts_fts(rowid, text) VALUES (new.id, new.text); END;

CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY, person TEXT NOT NULL, channel TEXT NOT NULL, chat TEXT NOT NULL,
  task TEXT NOT NULL, workdir TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued',
  created INTEGER NOT NULL, started INTEGER, finished INTEGER,
  result TEXT NOT NULL DEFAULT '', cost REAL, log_path TEXT NOT NULL DEFAULT '', turn_id INTEGER);

CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, started INTEGER NOT NULL, finished INTEGER,
  exit INTEGER, output TEXT NOT NULL DEFAULT '', delivered INTEGER NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS prompt_versions (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, content TEXT NOT NULL, hash TEXT NOT NULL, saved INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS replay (
  id INTEGER PRIMARY KEY, run TEXT NOT NULL, idx INTEGER NOT NULL, input TEXT NOT NULL,
  baseline TEXT NOT NULL DEFAULT '', engine TEXT NOT NULL, reply TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER, in_tok INTEGER, tools TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS replay_run ON replay(run, idx);

CREATE TABLE IF NOT EXISTS reminders (
  id INTEGER PRIMARY KEY, person TEXT NOT NULL, channel TEXT NOT NULL, chat TEXT NOT NULL,
  at INTEGER NOT NULL, text TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', created INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS reminders_due ON reminders(status, at);

CREATE TABLE IF NOT EXISTS routines (
  id INTEGER PRIMARY KEY, person TEXT NOT NULL, channel TEXT NOT NULL, chat TEXT NOT NULL,
  name TEXT NOT NULL, cron TEXT NOT NULL, task TEXT NOT NULL, notify TEXT NOT NULL DEFAULT 'notable',
  enabled INTEGER NOT NULL DEFAULT 1, created INTEGER NOT NULL, last_run INTEGER, last_result TEXT NOT NULL DEFAULT '');

CREATE TABLE IF NOT EXISTS systems (hash TEXT PRIMARY KEY, text TEXT NOT NULL, ts INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT NOT NULL, ts INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS shadow (
  id INTEGER PRIMARY KEY,
  turn_id INTEGER NOT NULL,                     -- the real (Claude) turn it shadows
  source TEXT NOT NULL,                         -- live | history
  variant TEXT NOT NULL,                        -- fast | think
  gear TEXT NOT NULL DEFAULT '', obs INTEGER NOT NULL DEFAULT 0, cold INTEGER NOT NULL DEFAULT 0,
  reply TEXT NOT NULL DEFAULT '', tools TEXT NOT NULL DEFAULT '', trace TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0,
  in_tok INTEGER NOT NULL DEFAULT 0, out_tok INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS shadow_turn ON shadow(turn_id);
`

type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(normal)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	db.Exec(`ALTER TABLE shadow ADD COLUMN rev TEXT NOT NULL DEFAULT ''`) // added 2026-10-11; fails harmlessly once present
	return &Store{DB: db}, nil
}

func now() int64 { return time.Now().Unix() }

// ---- kv ----

func (s *Store) Get(k string) string {
	var v string
	s.DB.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) Set(k, v string) {
	s.DB.Exec(`INSERT INTO kv(k,v,ts) VALUES(?,?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v, ts=excluded.ts`, k, v, now())
}

// ---- events ----

type Event struct {
	ID                            int64
	Channel, Chat, Sender, Person string
	GUID, Text, Images            string
	TS                            int64
}

// AddEvent stores an inbound message. A repeated GUID returns ok=false: some platforms
// re-posts the same message on every metadata change (read receipts, delivery).
func (s *Store) AddEvent(e *Event) (ok bool, err error) {
	var guid any
	if e.GUID != "" {
		guid = e.GUID
	}
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO events(channel,chat,sender,person,guid,text,images,ts) VALUES(?,?,?,?,?,?,?,?)`,
		e.Channel, e.Chat, e.Sender, e.Person, guid, e.Text, e.Images, e.TS)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	e.ID, _ = res.LastInsertId()
	return true, nil
}

func (s *Store) PendingEvents(person string, maxAge time.Duration) ([]*Event, error) {
	rows, err := s.DB.Query(`SELECT id,channel,chat,sender,person,COALESCE(guid,''),text,images,ts FROM events
		WHERE person=? AND status='new' AND ts>=? ORDER BY id LIMIT 20`, person, time.Now().Add(-maxAge).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e := &Event{}
		rows.Scan(&e.ID, &e.Channel, &e.Chat, &e.Sender, &e.Person, &e.GUID, &e.Text, &e.Images, &e.TS)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkEvents(ids []int64, status string, turn int64) {
	for _, id := range ids {
		s.DB.Exec(`UPDATE events SET status=?, turn_id=? WHERE id=?`, status, turn, id)
	}
}

// ---- turns ----

type Turn struct {
	ID                                  int64
	Person, Channel, Chat, Kind         string
	Started, Finished                   int64
	Engine, Status, Input, Reply, Tools string
	LatencyMS, InTok, OutTok            int64
	CacheRead, CacheWrite               int64
	Cost                                float64
	PromptHash, Error                   string
	Feedback                            int
	FeedbackNote, OutGUIDs, Attachments string
}

const turnCols = `id,person,channel,chat,kind,started,COALESCE(finished,0),engine,status,input,reply,tools,
	COALESCE(latency_ms,0),COALESCE(in_tok,0),COALESCE(out_tok,0),COALESCE(cache_read,0),COALESCE(cache_write,0),
	COALESCE(cost,0),prompt_hash,error,feedback,feedback_note,out_guids,attachments`

func scanTurn(r interface{ Scan(...any) error }) (*Turn, error) {
	t := &Turn{}
	err := r.Scan(&t.ID, &t.Person, &t.Channel, &t.Chat, &t.Kind, &t.Started, &t.Finished, &t.Engine, &t.Status,
		&t.Input, &t.Reply, &t.Tools, &t.LatencyMS, &t.InTok, &t.OutTok, &t.CacheRead, &t.CacheWrite, &t.Cost,
		&t.PromptHash, &t.Error, &t.Feedback, &t.FeedbackNote, &t.OutGUIDs, &t.Attachments)
	return t, err
}

func (s *Store) StartTurn(person, channel, chat, kind, input string) int64 {
	res, err := s.DB.Exec(`INSERT INTO turns(person,channel,chat,kind,started,input) VALUES(?,?,?,?,?,?)`,
		person, channel, chat, kind, now(), input)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *Store) FinishTurn(t *Turn) error {
	_, err := s.DB.Exec(`UPDATE turns SET finished=?, engine=?, status=?, reply=?, tools=?, latency_ms=?, in_tok=?, out_tok=?,
		cache_read=?, cache_write=?, cost=?, prompt_hash=?, error=?, attachments=? WHERE id=?`,
		now(), t.Engine, t.Status, t.Reply, t.Tools, t.LatencyMS, t.InTok, t.OutTok, t.CacheRead, t.CacheWrite, t.Cost,
		t.PromptHash, t.Error, t.Attachments, t.ID)
	return err
}

// AddNotice records something the assistant said unprompted (job report, scheduled
// message, failover notice) so it shows up in her own recent context.
func (s *Store) AddNotice(person, channel, chat, kind, text string) int64 {
	res, err := s.DB.Exec(`INSERT INTO turns(person,channel,chat,kind,started,finished,status,reply) VALUES(?,?,?,?,?,?, 'ok', ?)`,
		person, channel, chat, kind, now(), now(), text)
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *Store) AddOutGUID(turn int64, guid string) {
	if guid != "" && turn > 0 {
		s.DB.Exec(`UPDATE turns SET out_guids = out_guids || ? || ' ' WHERE id=?`, guid, turn)
	}
}

func (s *Store) TurnByOutGUID(guid string) int64 {
	var id int64
	if guid != "" {
		s.DB.QueryRow(`SELECT id FROM turns WHERE instr(out_guids, ?) > 0 ORDER BY id DESC LIMIT 1`, guid+" ").Scan(&id)
	}
	return id
}

func (s *Store) SetFeedback(turn int64, score int, note string) {
	if note != "" {
		s.DB.Exec(`UPDATE turns SET feedback=?, feedback_note=? WHERE id=?`, score, note, turn)
	} else {
		s.DB.Exec(`UPDATE turns SET feedback=? WHERE id=?`, score, turn)
	}
}

func (s *Store) Turn(id int64) (*Turn, error) {
	return scanTurn(s.DB.QueryRow(`SELECT `+turnCols+` FROM turns WHERE id=?`, id))
}

// Recent returns a person's last n conversation turns (oldest first) after the
// context epoch set by /new.
func (s *Store) Recent(person string, n int) ([]*Turn, error) {
	epoch, _ := parseInt(s.Get("epoch:" + person))
	rows, err := s.DB.Query(`SELECT `+turnCols+` FROM turns WHERE person=? AND kind IN ('chat','job','notice','external','action')
		AND status='ok' AND id>? ORDER BY id DESC LIMIT ?`, person, epoch, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Turn
	for rows.Next() {
		if t, err := scanTurn(rows); err == nil {
			out = append([]*Turn{t}, out...)
		}
	}
	return out, rows.Err()
}

// TurnsFrom returns a person's conversation turns with id >= from (and after
// the /new epoch), oldest first. It backs the append-only recent window.
// Dashboard console turns are tests and stay out of it.
func (s *Store) TurnsFrom(person string, from int64, limit int) ([]*Turn, error) {
	epoch, _ := parseInt(s.Get("epoch:" + person))
	if from <= epoch {
		from = epoch + 1
	}
	rows, err := s.DB.Query(`SELECT `+turnCols+` FROM turns WHERE person=? AND kind IN ('chat','job','notice','external','action')
		AND status='ok' AND channel<>'console' AND id>=? ORDER BY id LIMIT ?`, person, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Turn
	for rows.Next() {
		if t, err := scanTurn(rows); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

type TurnFilter struct {
	Person, Kind string
	Feedback     bool // only turns with feedback
	Errors       bool
	BeforeID     int64
	AfterID      int64
	Limit        int
}

func (s *Store) Turns(f TurnFilter) ([]*Turn, error) {
	q := `SELECT ` + turnCols + ` FROM turns WHERE 1=1`
	var args []any
	if f.Person != "" {
		q += ` AND person=?`
		args = append(args, f.Person)
	}
	if f.Kind != "" {
		q += ` AND kind=?`
		args = append(args, f.Kind)
	} else {
		q += ` AND kind<>'replay'`
	}
	if f.Feedback {
		q += ` AND (feedback<>0 OR feedback_note<>'')`
	}
	if f.Errors {
		q += ` AND status NOT IN ('ok','running')`
	}
	if f.BeforeID > 0 {
		q += ` AND id<?`
		args = append(args, f.BeforeID)
	}
	if f.AfterID > 0 {
		q += ` AND id>?`
		args = append(args, f.AfterID)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	q += fmt.Sprintf(` ORDER BY id DESC LIMIT %d`, f.Limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Turn
	for rows.Next() {
		if t, err := scanTurn(rows); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// SearchTurns is full-text recall over past conversation.
func (s *Store) SearchTurns(person, query string, limit int) ([]*Turn, error) {
	q := ftsQuery(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.DB.Query(`SELECT `+turnCols+` FROM turns WHERE person=? AND kind NOT IN ('replay','system') AND id IN
		(SELECT rowid FROM turns_fts WHERE turns_fts MATCH ? ORDER BY rank LIMIT 200) ORDER BY id DESC LIMIT ?`, person, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Turn
	for rows.Next() {
		if t, err := scanTurn(rows); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// ---- trace ----

const traceCap = 16 << 10

func (s *Store) Trace(turn int64, seq int, kind, name, body string) {
	if turn <= 0 {
		return
	}
	if len(body) > traceCap {
		body = body[:traceCap] + fmt.Sprintf("\n…[%d bytes truncated]", len(body)-traceCap)
	}
	s.DB.Exec(`INSERT OR REPLACE INTO trace(turn_id,seq,kind,name,body,ts) VALUES(?,?,?,?,?,?)`, turn, seq, kind, name, body, now())
}

type TraceRow struct {
	Seq              int
	Kind, Name, Body string
	TS               int64
}

func (s *Store) TraceRows(turn int64) ([]TraceRow, error) {
	rows, err := s.DB.Query(`SELECT seq,kind,name,body,ts FROM trace WHERE turn_id=? ORDER BY seq LIMIT 500`, turn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TraceRow
	for rows.Next() {
		var r TraceRow
		rows.Scan(&r.Seq, &r.Kind, &r.Name, &r.Body, &r.TS)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- facts ----

type Fact struct {
	ID           int64
	Person, Text string
	Pinned       bool
	Source       string
	Created      int64
}

func (s *Store) AddFact(person, text string, pinned bool, source string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("empty fact")
	}
	var dup int64
	s.DB.QueryRow(`SELECT id FROM facts WHERE person=? AND text=?`, person, text).Scan(&dup)
	if dup > 0 {
		return dup, nil
	}
	res, err := s.DB.Exec(`INSERT INTO facts(person,text,pinned,source,created) VALUES(?,?,?,?,?)`, person, text, pinned, source, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteFact(person string, id int64) bool {
	res, _ := s.DB.Exec(`DELETE FROM facts WHERE id=? AND person=?`, id, person)
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) PinFact(id int64, pinned bool) {
	s.DB.Exec(`UPDATE facts SET pinned=? WHERE id=?`, pinned, id)
}

func (s *Store) Facts(person string, pinnedOnly bool) ([]Fact, error) {
	q := `SELECT id,person,text,pinned,source,created FROM facts WHERE person=?`
	if pinnedOnly {
		q += ` AND pinned=1`
	}
	rows, err := s.DB.Query(q+` ORDER BY pinned DESC, id LIMIT 500`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		rows.Scan(&f.ID, &f.Person, &f.Text, &f.Pinned, &f.Source, &f.Created)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SearchFacts returns unpinned facts relevant to text (pinned ones are always in the prompt).
func (s *Store) SearchFacts(person, text string, limit int) ([]Fact, error) {
	q := ftsQuery(text)
	if q == "" {
		return nil, nil
	}
	rows, err := s.DB.Query(`SELECT f.id,f.person,f.text,f.pinned,f.source,f.created FROM facts_fts x JOIN facts f ON f.id=x.rowid
		WHERE facts_fts MATCH ? AND f.person=? AND f.pinned=0 ORDER BY rank LIMIT ?`, q, person, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		rows.Scan(&f.ID, &f.Person, &f.Text, &f.Pinned, &f.Source, &f.Created)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- prompt versions ----

func (s *Store) SavePromptVersion(name, content, hash string) {
	var last string
	s.DB.QueryRow(`SELECT hash FROM prompt_versions WHERE name=? ORDER BY id DESC LIMIT 1`, name).Scan(&last)
	if last != hash {
		s.DB.Exec(`INSERT INTO prompt_versions(name,content,hash,saved) VALUES(?,?,?,?)`, name, content, hash, now())
	}
}

type PromptVersion struct {
	ID                  int64
	Name, Content, Hash string
	Saved               int64
}

func (s *Store) PromptVersions(name string, limit int) ([]PromptVersion, error) {
	rows, err := s.DB.Query(`SELECT id,name,content,hash,saved FROM prompt_versions WHERE name=? ORDER BY id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PromptVersion
	for rows.Next() {
		var v PromptVersion
		rows.Scan(&v.ID, &v.Name, &v.Content, &v.Hash, &v.Saved)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) PromptVersion(id int64) (*PromptVersion, error) {
	v := &PromptVersion{}
	err := s.DB.QueryRow(`SELECT id,name,content,hash,saved FROM prompt_versions WHERE id=?`, id).Scan(&v.ID, &v.Name, &v.Content, &v.Hash, &v.Saved)
	return v, err
}

// ---- reminders ----

type Reminder struct {
	ID                          int64
	Person, Channel, Chat, Text string
	At                          int64
}

func (s *Store) AddReminder(r *Reminder) (int64, error) {
	res, err := s.DB.Exec(`INSERT INTO reminders(person,channel,chat,at,text,created) VALUES(?,?,?,?,?,?)`,
		r.Person, r.Channel, r.Chat, r.At, r.Text, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) reminders(q string, args ...any) []Reminder {
	rows, err := s.DB.Query(`SELECT id,person,channel,chat,at,text FROM reminders WHERE `+q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		rows.Scan(&r.ID, &r.Person, &r.Channel, &r.Chat, &r.At, &r.Text)
		out = append(out, r)
	}
	return out
}

// PendingReminders lists a person's upcoming reminders, soonest first.
func (s *Store) PendingReminders(person string, limit int) []Reminder {
	return s.reminders(`person=? AND status='pending' ORDER BY at LIMIT ?`, person, limit)
}

// DueReminders claims reminders whose time has come (marking them sent).
func (s *Store) DueReminders() []Reminder {
	due := s.reminders(`status='pending' AND at<=? ORDER BY at LIMIT 20`, now())
	for _, r := range due {
		s.DB.Exec(`UPDATE reminders SET status='sent' WHERE id=?`, r.ID)
	}
	return due
}

func (s *Store) CancelReminder(person string, id int64) bool {
	res, _ := s.DB.Exec(`UPDATE reminders SET status='cancelled' WHERE id=? AND person=? AND status='pending'`, id, person)
	n, _ := res.RowsAffected()
	return n > 0
}

// ---- system prompts (stored once per distinct version, for the turn inspector) ----

func (s *Store) SaveSystem(hash, text string) {
	s.DB.Exec(`INSERT OR IGNORE INTO systems(hash,text,ts) VALUES(?,?,?)`, hash, text, now())
}

func (s *Store) System(hash string) string {
	var t string
	s.DB.QueryRow(`SELECT text FROM systems WHERE hash=?`, hash).Scan(&t)
	return t
}

// ---- maintenance ----

// Prune keeps the database from growing without bound: full traces and old
// events go after traceDays; turns, facts and jobs stay (they're small text).
func (s *Store) Prune(ctx context.Context, traceDays int) {
	cut := time.Now().AddDate(0, 0, -traceDays).Unix()
	s.DB.ExecContext(ctx, `DELETE FROM trace WHERE ts<?`, cut)
	s.DB.ExecContext(ctx, `DELETE FROM events WHERE ts<?`, cut)
	s.DB.ExecContext(ctx, `DELETE FROM runs WHERE started<?`, cut)
	s.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
}

func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// ---- helpers ----

func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}

// ftsQuery turns free text into an OR query of its meaningful words, so FTS5
// syntax characters in user text can't break the MATCH.
func ftsQuery(text string) string {
	var words []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) < 3 || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, `"`+w+`"`)
		if len(words) == 12 {
			break
		}
	}
	return strings.Join(words, " OR ")
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are but not you all any can her was one our out day get has him his how man new now old see two way who boy did its let put say she too use what with that this have from they will would there their been were when your which them then than some could about into just like what's also yeah yes okay hey yo lol tell know want need really think`) {
		stop[w] = true
	}
}
