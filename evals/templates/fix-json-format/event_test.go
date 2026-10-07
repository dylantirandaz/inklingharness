package fixture

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestEncode(t *testing.T) {
	created := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		event Event
		want  string
	}{
		{Event{ID: "e1", CreatedAt: created, Tags: []string{"a"}, Note: "hi"}, `{"id":"e1","created_at":"2024-01-02T03:04:05Z","tags":["a"],"note":"hi"}`},
		{Event{ID: "e2", CreatedAt: created}, `{"id":"e2","created_at":"2024-01-02T03:04:05Z"}`},
	}
	for _, c := range cases {
		data, err := json.Marshal(c.event)
		if err != nil || string(data) != c.want {
			t.Errorf("Marshal = %s, %v; want %s", data, err, c.want)
		}
	}
}

func TestDecode(t *testing.T) {
	var event Event
	input := `{"id":"e3","created_at":"2024-05-06T07:08:09Z","tags":["x","y"]}`
	if err := json.Unmarshal([]byte(input), &event); err != nil {
		t.Fatal(err)
	}
	if event.ID != "e3" || !event.CreatedAt.Equal(time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)) || !slices.Equal(event.Tags, []string{"x", "y"}) {
		t.Fatalf("Unmarshal = %+v", event)
	}
}
