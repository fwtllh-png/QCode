package config

import (
	"errors"
	"testing"
	"time"
)

func TestDeletedEventRetentionConfiguration(t *testing.T) {
	for _, value := range []string{"0s", "1ns", "24h"} {
		t.Run(value, func(t *testing.T) {
			path := writeConfig(t, "[state]\ndeleted_event_retention = \""+value+"\"\narchive_deleted_events = true\n")
			snapshot, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
			if err != nil {
				t.Fatal(err)
			}
			want, _ := time.ParseDuration(value)
			if snapshot.Config.State.DeletedEventRetention != want || !snapshot.Config.State.ArchiveDeletedEvents ||
				snapshot.Provenance[fieldDeletedEventRetention] != SourceFile {
				t.Fatalf("file retention: %+v", snapshot)
			}
			snapshot, err = Load(LoadOptions{Path: path, LookupEnv: envLookup(map[string]string{
				"QCODE_STATE_DELETED_EVENT_RETENTION": "2h",
				"QCODE_STATE_ARCHIVE_DELETED_EVENTS":  "false",
			})})
			if err != nil || snapshot.Config.State.DeletedEventRetention != 2*time.Hour ||
				snapshot.Config.State.ArchiveDeletedEvents || snapshot.Provenance[fieldDeletedEventRetention] != SourceEnv {
				t.Fatalf("environment retention: %+v %v", snapshot, err)
			}
		})
	}
	for _, value := range []string{"-1ns", "invalid", "999999999999999999999h"} {
		path := writeConfig(t, "[state]\ndeleted_event_retention = \""+value+"\"\n")
		_, err := Load(LoadOptions{Path: path, LookupEnv: envLookup(nil)})
		var field *FieldError
		if !errors.As(err, &field) || field.Field != fieldDeletedEventRetention {
			t.Fatalf("invalid retention %q: %v", value, err)
		}
	}
}
