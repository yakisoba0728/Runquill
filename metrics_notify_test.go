package main

import (
	"strings"
	"testing"
)

func TestMetricsReportNotificationCountersOnlyWithNotifier(t *testing.T) {
	var without strings.Builder
	(&fleetMetrics{}).writePrometheus(&without)
	if strings.Contains(without.String(), "notifications_") {
		t.Fatalf("notification counters without a notifier:\n%s", without.String())
	}
	srv, _ := notifyTestServer(t, nil)
	n := notifyTestNew(t, notifyConfig{URL: srv.URL})
	n.Notify(notifyEvent{Kind: notifyJobFailed, Repository: "owner/repo", JobID: 1})
	notifyTestClose(t, n)
	var with strings.Builder
	(&fleetMetrics{notify: n}).writePrometheus(&with)
	for _, line := range []string{"runquill_notifications_sent_total 1", "runquill_notifications_failed_total 0",
		"runquill_notifications_dropped_total 0"} {
		if !strings.Contains(with.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, with.String())
		}
	}
	if strings.Contains(with.String(), srv.URL) {
		t.Fatal("metrics exposed the notify URL")
	}
}
