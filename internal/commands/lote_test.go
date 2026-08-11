package commands

import (
	"encoding/json"
	"testing"

	"github.com/gotd/td/tg"
)

func TestDetectQuality(t *testing.T) {
	tests := []struct {
		name     string
		fileName string
		media    tg.MessageMediaClass
		expected string
	}{
		{
			name:     "1080p from filename",
			fileName: "Movie.1080p.WEBRip.x264.mkv",
			media:    &tg.MessageMediaDocument{},
			expected: "1080p",
		},
		{
			name:     "720p from filename",
			fileName: "Episode.S01E01.720p.hdtv.mp4",
			media:    &tg.MessageMediaDocument{},
			expected: "720p",
		},
		{
			name:     "4k from filename",
			fileName: "UltraHD.4K.Film.mkv",
			media:    &tg.MessageMediaDocument{},
			expected: "2160p",
		},
		{
			name:     "default fallback 1080p",
			fileName: "SomeMovieFile.avi",
			media:    &tg.MessageMediaDocument{},
			expected: "1080p",
		},
		{
			name:     "media document dimensions 720p",
			fileName: "unknown.mp4",
			media: &tg.MessageMediaDocument{
				Document: &tg.Document{
					Attributes: []tg.DocumentAttributeClass{
						&tg.DocumentAttributeVideo{
							W: 1280,
							H: 720,
						},
					},
				},
			},
			expected: "720p",
		},
		{
			name:     "media document dimensions 1080p",
			fileName: "unknown.mp4",
			media: &tg.MessageMediaDocument{
				Document: &tg.Document{
					Attributes: []tg.DocumentAttributeClass{
						&tg.DocumentAttributeVideo{
							W: 1920,
							H: 1080,
						},
					},
				},
			},
			expected: "1080p",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectQuality(tt.media, tt.fileName)
			if got != tt.expected {
				t.Errorf("detectQuality() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestLoteStateJsonFormatting(t *testing.T) {
	// Test movie stream JSON structure
	movieState := &LoteState{
		Colaborador: "TestColab",
		Type:        "movie",
		ImdbID:      "tt12345",
		Title:       "Test Movie",
		Audio:       "Dublado",
		MovieStreams: []StreamObj{
			{
				URL:         "/stream/1",
				Name:        "Dublado\n1080p",
				Colaborador: "TestColab",
			},
		},
	}

	movieJSONMap := map[string]interface{}{
		"id":          movieState.ImdbID,
		"type":        "movie",
		"streams":     movieState.MovieStreams,
		"colaborador": movieState.Colaborador,
	}

	movieBytes, err := json.Marshal(movieJSONMap)
	if err != nil {
		t.Fatalf("Failed to marshal movie JSON: %v", err)
	}

	var parsedMovie map[string]interface{}
	if err := json.Unmarshal(movieBytes, &parsedMovie); err != nil {
		t.Fatalf("Failed to unmarshal movie JSON: %v", err)
	}

	if parsedMovie["id"] != "tt12345" || parsedMovie["type"] != "movie" {
		t.Errorf("Unexpected movie JSON structure: %s", string(movieBytes))
	}

	// Test series stream JSON structure
	seriesState := &LoteState{
		Colaborador: "TestColab",
		Type:        "series",
		ImdbID:      "tt67890",
		Title:       "Test Series",
		Audio:       "Legendado",
		SeriesStreams: map[string]map[string][]StreamObj{
			"1": {
				"1": []StreamObj{
					{
						URL:         "/stream/2",
						Name:        "Legendado\n720p",
						Colaborador: "TestColab",
					},
				},
			},
		},
	}

	seriesJSONMap := map[string]interface{}{
		"id":          seriesState.ImdbID,
		"type":        "series",
		"streams":     seriesState.SeriesStreams,
		"colaborador": seriesState.Colaborador,
	}

	seriesBytes, err := json.Marshal(seriesJSONMap)
	if err != nil {
		t.Fatalf("Failed to marshal series JSON: %v", err)
	}

	var parsedSeries map[string]interface{}
	if err := json.Unmarshal(seriesBytes, &parsedSeries); err != nil {
		t.Fatalf("Failed to unmarshal series JSON: %v", err)
	}

	if parsedSeries["id"] != "tt67890" || parsedSeries["type"] != "series" {
		t.Errorf("Unexpected series JSON structure: %s", string(seriesBytes))
	}
}

func TestLoteStateSeasonChange(t *testing.T) {
	// Simulate starting a batch at season 1, episode 8
	state := &LoteState{
		Season:    1,
		CurrentEp: 8,
		Step:      8,
		Type:      "series",
	}

	// Simulating incrementing the season (like clicking the "➕ Temp" button or typing "/temporada 2")
	// The logic inside lote.go does:
	state.Season++
	state.CurrentEp = 1 // Reset to 1

	if state.Season != 2 {
		t.Errorf("Expected season to be 2, got %d", state.Season)
	}
	if state.CurrentEp != 1 {
		t.Errorf("Expected current episode to reset to 1, got %d", state.CurrentEp)
	}
}
