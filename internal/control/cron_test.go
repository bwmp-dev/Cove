package control

import (
	"context"
	"testing"
	"time"
)

func TestCronCalendarAndValidation(t *testing.T) {
	for _, tc := range []struct{ expression, after, want string }{
		{"*/15 * * * *", "2026-11-19T10:14:59Z", "2026-11-19T10:15:00Z"},
		{"0 3 * * *", "2026-11-19T03:00:00Z", "2026-11-20T03:00:00Z"},
		{"0 0 29 2 *", "2026-11-19T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"0 9 * * 1-5", "2026-11-20T09:00:00Z", "2026-11-23T09:00:00Z"},
		{"0 0 1 * 0", "2026-11-19T00:00:00Z", "2026-11-22T00:00:00Z"},
	} {
		schedule, err := parseCron(tc.expression)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := time.Parse(time.RFC3339, tc.after)
		next, err := schedule.next(after)
		if err != nil || next.Format(time.RFC3339) != tc.want {
			t.Fatal(tc, next, err)
		}
	}
	for _, expression := range []string{"", "* * * *", "60 * * * *", "*/0 * * * *", "* * * * 7", "* * 0 * *", "* * * JAN *", "1-0 * * * *", "* * * * 1,"} {
		if _, err := parseCron(expression); err == nil {
			t.Fatal("accepted", expression)
		}
	}
	impossible, err := parseCron("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = impossible.next(time.Now()); err == nil {
		t.Fatal("impossible date accepted")
	}
}

func TestScheduledJobsSkipOverlapAndManualStop(t *testing.T) {
	f := newAccountFixture(t)
	f.sql("INSERT INTO nodes(id,name,address,port,public_key,private_key,state) VALUES('cron-node','Node','localhost',22,'',$1,'ready')", []byte{})
	f.sql("INSERT INTO projects(id,name,team_id) VALUES('cron-project','Project','default')")
	input := workloadInput{Name: "Job", NodeID: "cron-node", ProjectID: "cron-project", Kind: "job", JobService: "app", Compose: "services:\n  app:\n    image: busybox:1.37\n    command: ['true']\n"}
	id := jsonString(t, f.call("root", "default", "POST", "/api/workloads", input, 201), "id")
	path := "/api/workloads/" + id + "/schedule"
	f.call("root", "default", "PUT", path, map[string]string{"cron": "*/5 * * * *"}, 200)
	due := func() { f.sql("UPDATE workloads SET next_job_at=now()-interval '1 day' WHERE id=$1", id) }
	due()
	if err := f.s.scheduleJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	due()
	if err := f.s.scheduleJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	f.sql("UPDATE operations SET state='succeeded' WHERE workload_id=$1", id)
	f.sql("UPDATE workloads SET manually_stopped=true WHERE id=$1", id)
	due()
	if err := f.s.scheduleJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE workload_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	input.ProjectID = ""
	input.Kind = "service"
	input.JobService = ""
	f.call("root", "default", "PUT", "/api/workloads/"+id, input, 200)
	f.call("root", "default", "PUT", path, map[string]string{"cron": "* * * * *"}, 400)
	var cron string
	if err := f.s.DB.QueryRow(context.Background(), "SELECT job_cron FROM workloads WHERE id=$1", id).Scan(&cron); err != nil || cron != "" {
		t.Fatal(cron, err)
	}
}
