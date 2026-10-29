package store

import "time"

// Routine is a recurring task the assistant set up itself ("every morning check X").
type Routine struct {
	ID                          int64
	Person, Channel, Chat, Name string
	Cron, Task, Notify          string // notify: "always" | "notable"
	Enabled                     bool
	Created, LastRun            int64
	LastResult                  string
}

const routineCols = `id,person,channel,chat,name,cron,task,notify,enabled,created,COALESCE(last_run,0),last_result`

func scanRoutine(r interface{ Scan(...any) error }) (Routine, error) {
	var x Routine
	err := r.Scan(&x.ID, &x.Person, &x.Channel, &x.Chat, &x.Name, &x.Cron, &x.Task, &x.Notify, &x.Enabled, &x.Created, &x.LastRun, &x.LastResult)
	return x, err
}

func (s *Store) AddRoutine(r *Routine) (int64, error) {
	res, err := s.DB.Exec(`INSERT INTO routines(person,channel,chat,name,cron,task,notify,created) VALUES(?,?,?,?,?,?,?,?)`,
		r.Person, r.Channel, r.Chat, r.Name, r.Cron, r.Task, r.Notify, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Routines lists routines; person "" means everyone's.
func (s *Store) Routines(person string, enabledOnly bool) []Routine {
	q := `SELECT ` + routineCols + ` FROM routines WHERE (?='' OR person=?)`
	if enabledOnly {
		q += ` AND enabled=1`
	}
	rows, err := s.DB.Query(q+` ORDER BY id LIMIT 100`, person, person)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Routine
	for rows.Next() {
		if x, err := scanRoutine(rows); err == nil {
			out = append(out, x)
		}
	}
	return out
}

func (s *Store) Routine(id int64) (Routine, error) {
	return scanRoutine(s.DB.QueryRow(`SELECT `+routineCols+` FROM routines WHERE id=?`, id))
}

func (s *Store) SetRoutineEnabled(person string, id int64, on bool) bool {
	res, _ := s.DB.Exec(`UPDATE routines SET enabled=? WHERE id=? AND (?='' OR person=?)`, on, id, person, person)
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) DeleteRoutine(person string, id int64) bool {
	res, _ := s.DB.Exec(`DELETE FROM routines WHERE id=? AND (?='' OR person=?)`, id, person, person)
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) RoutineRan(id int64, result string) {
	if len(result) > 2000 {
		result = result[:2000]
	}
	s.DB.Exec(`UPDATE routines SET last_run=?, last_result=? WHERE id=?`, time.Now().Unix(), result, id)
}

// TurnsSince returns a person's conversation (chat turns and notices) after
// a given time, oldest first, for the heartbeat's review.
func (s *Store) TurnsSince(person string, since int64, limit int) ([]*Turn, error) {
	// Console turns are dashboard tests, never something to learn from.
	rows, err := s.DB.Query(`SELECT `+turnCols+` FROM turns WHERE person=? AND kind IN ('chat','notice','external','action') AND status='ok'
		AND channel<>'console' AND started>? ORDER BY id DESC LIMIT ?`, person, since, limit)
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
