package store

// Shadow runs: the local model answering a real turn in parallel with
// Claude, never delivered, kept for comparison (the dashboard's Shadow page).

type Shadow struct {
	ID, TurnID               int64
	Source, Variant, Gear    string
	Rev                      string // prompt/engine revision, so tuning rounds compare cleanly
	OBS, Cold                bool
	Reply, Tools, Trace      string
	Error                    string
	LatencyMS, InTok, OutTok int64
	Verdict, Note            string
	TS                       int64
}

func (s *Store) AddShadow(x *Shadow) int64 {
	res, err := s.DB.Exec(`INSERT INTO shadow(turn_id,source,variant,gear,obs,cold,reply,tools,trace,error,latency_ms,in_tok,out_tok,rev,ts)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, x.TurnID, x.Source, x.Variant, x.Gear, x.OBS, x.Cold, x.Reply, x.Tools, x.Trace,
		x.Error, x.LatencyMS, x.InTok, x.OutTok, x.Rev, now())
	if err != nil {
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

func (s *Store) SetShadowVerdict(id int64, verdict, note string) {
	s.DB.Exec(`UPDATE shadow SET verdict=?, note=? WHERE id=?`, verdict, note, id)
}

// ShadowDone reports whether a turn already has a good shadow row for this
// variant, revision and gear (model): a model trial must not count another
// model's rows as its own.
func (s *Store) ShadowDone(turn int64, variant, rev, gear string) bool { // any source
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM shadow WHERE turn_id=? AND variant=? AND rev=? AND gear=? AND error=''`, turn, variant, rev, gear).Scan(&n)
	return n > 0
}

const shadowCols = `id,turn_id,source,variant,gear,obs,cold,reply,tools,trace,error,latency_ms,in_tok,out_tok,verdict,note,ts,rev`

func scanShadow(sc interface{ Scan(...any) error }) (*Shadow, error) {
	x := &Shadow{}
	err := sc.Scan(&x.ID, &x.TurnID, &x.Source, &x.Variant, &x.Gear, &x.OBS, &x.Cold, &x.Reply, &x.Tools, &x.Trace,
		&x.Error, &x.LatencyMS, &x.InTok, &x.OutTok, &x.Verdict, &x.Note, &x.TS, &x.Rev)
	return x, err
}

// Shadows returns shadow rows, newest turns first. source "" = all.
func (s *Store) Shadows(source string, limit int) ([]*Shadow, error) {
	rows, err := s.DB.Query(`SELECT `+shadowCols+` FROM shadow WHERE (?='' OR source=?) ORDER BY turn_id DESC, rev DESC, variant LIMIT ?`, source, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Shadow
	for rows.Next() {
		if x, err := scanShadow(rows); err == nil {
			out = append(out, x)
		}
	}
	return out, rows.Err()
}

func (s *Store) ShadowsForTurn(turn int64) []*Shadow {
	rows, err := s.DB.Query(`SELECT `+shadowCols+` FROM shadow WHERE turn_id=? ORDER BY variant`, turn)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Shadow
	for rows.Next() {
		if x, err := scanShadow(rows); err == nil {
			out = append(out, x)
		}
	}
	return out
}

// ShadowStat summarizes one variant on one gear.
type ShadowStat struct {
	Source, Variant, Gear, Rev         string
	N, Errors, Empty, Cold             int
	AvgMS, P50MS, P90MS, AvgIn, AvgOut int64
	Better, Same, Worse, Wrong         int
}

func (s *Store) ShadowStats() []ShadowStat {
	rows, err := s.DB.Query(`SELECT source, variant, gear, rev, COUNT(*), SUM(error<>''), SUM(error='' AND reply=''), SUM(cold),
		CAST(AVG(CASE WHEN error='' THEN latency_ms END) AS INT), CAST(AVG(in_tok) AS INT), CAST(AVG(out_tok) AS INT),
		SUM(verdict='better'), SUM(verdict='same'), SUM(verdict='worse'), SUM(verdict='wrong')
		FROM shadow GROUP BY source, variant, gear, rev ORDER BY source, rev, variant, gear`)
	if err != nil {
		return nil
	}
	var out []ShadowStat
	for rows.Next() {
		var x ShadowStat
		var avg *int64
		rows.Scan(&x.Source, &x.Variant, &x.Gear, &x.Rev, &x.N, &x.Errors, &x.Empty, &x.Cold, &avg, &x.AvgIn, &x.AvgOut,
			&x.Better, &x.Same, &x.Worse, &x.Wrong)
		if avg != nil {
			x.AvgMS = *avg
		}
		out = append(out, x)
	}
	rows.Close()
	for i := range out {
		x := &out[i]
		x.P50MS, x.P90MS = s.shadowPct(x, 0.5), s.shadowPct(x, 0.9)
	}
	return out
}

func (s *Store) shadowPct(x *ShadowStat, p float64) int64 {
	const where = `FROM shadow WHERE source=? AND variant=? AND gear=? AND rev=? AND error=''`
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) `+where, x.Source, x.Variant, x.Gear, x.Rev).Scan(&n)
	if n == 0 {
		return 0
	}
	var v int64
	s.DB.QueryRow(`SELECT latency_ms `+where+` ORDER BY latency_ms LIMIT 1 OFFSET ?`, x.Source, x.Variant, x.Gear, x.Rev, int(float64(n-1)*p)).Scan(&v)
	return v
}
