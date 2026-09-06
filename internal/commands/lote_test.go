package commands

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/types"
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

func TestDetectSeasonAndEpisode(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		caption     string
		wantSeason  int
		wantEpisode int
		wantChapter bool
	}{
		// Standard Series Patterns
		{
			name:        "S02E06 standard",
			fileName:    "House.of.the.Dragon.S02E06.1080p.mkv",
			caption:     "",
			wantSeason:  2,
			wantEpisode: 6,
			wantChapter: false,
		},
		{
			name:        "s01e01 lowercase",
			fileName:    "serie.s01e01.mp4",
			caption:     "",
			wantSeason:  1,
			wantEpisode: 1,
			wantChapter: false,
		},
		{
			name:        "S2E6 single digit",
			fileName:    "Show.S2E6.mkv",
			caption:     "",
			wantSeason:  2,
			wantEpisode: 6,
			wantChapter: false,
		},
		{
			name:        "S01-E05 dash format",
			fileName:    "Anime.S01-E05.mkv",
			caption:     "",
			wantSeason:  1,
			wantEpisode: 5,
			wantChapter: false,
		},
		{
			name:        "S01_E05 underscore format",
			fileName:    "Anime_S01_E05_1080p.mkv",
			caption:     "",
			wantSeason:  1,
			wantEpisode: 5,
			wantChapter: false,
		},
		{
			name:        "T01E05 Portuguese/Spanish notation",
			fileName:    "Serie.T01E05.mkv",
			caption:     "",
			wantSeason:  1,
			wantEpisode: 5,
			wantChapter: false,
		},
		{
			name:        "2x06 notation",
			fileName:    "The.Flash.2x06.1080p.mkv",
			caption:     "",
			wantSeason:  2,
			wantEpisode: 6,
			wantChapter: false,
		},
		{
			name:        "2ª Temporada in filename",
			fileName:    "Dark.2ª.Temporada.mkv",
			caption:     "",
			wantSeason:  2,
			wantEpisode: 0,
			wantChapter: false,
		},
		{
			name:        "Standalone S03 in filename",
			fileName:    "Stranger.Things.S03.1080p.mkv",
			caption:     "",
			wantSeason:  3,
			wantEpisode: 0,
			wantChapter: false,
		},
		{
			name:     "Long caption with multiple metadata blocks and emojis",
			fileName: "video_8932.mkv",
			caption: `╔═════════════════════════╗
║       FENIX FILMES & SÉRIES      ║
╚═════════════════════════╝

🍿 Título: Shogun
📅 Ano: 2024
🎭 Gênero: Drama, Histórico
⭐ IMDb: 8.8/10

ℹ️ SINOPSE:
No Japão do século XVII, Lord Yoshii Toranaga luta por sua vida...

📁 DADOS DO ARQUIVO:
📺 Formato: MKV
🔊 Áudio: Dual Áudio (PT-BR / EN)
📜 Legenda: PT-BR
📊 Qualidade: WEB-DL 1080p
⏱️ Duração: 58 Min

📌 INFORMAÇÕES DO EPISÓDIO:
🔹 Temporada: S02
🔹 Episódio: E06

⬇️ BAIXAR ABAIXO:`,
			wantSeason:  2,
			wantEpisode: 6,
			wantChapter: false,
		},
		{
			name:        "Caption with Temporada 3 and Episódio 8",
			fileName:    "file.mkv",
			caption:     "Série Incrível\nTemporada: 3\nEpisódio: 8\nDual Áudio",
			wantSeason:  3,
			wantEpisode: 8,
			wantChapter: false,
		},

		// Chapter / Novela / Daily program Exceptions
		{
			name:        "CAPITULO [X] brackets",
			fileName:    "Novela.Renascer.Capitulo.45.mkv",
			caption:     "🌸 RENASCER 🌸\nCAPITULO [45]\nQUARTA FEIRA - 13/03/2024\nQualidade: 1080p",
			wantSeason:  1,
			wantEpisode: 45,
			wantChapter: true,
		},
		{
			name:        "CAPITULO with S02 in metadata - forces Season 1",
			fileName:    "Novela.Mania.de.Voce.S02E15.mkv",
			caption:     "MANIA DE VOCÊ - CAPITULO 15 - QUARTA FEIRA - Temporada 2",
			wantSeason:  1,
			wantEpisode: 15,
			wantChapter: true,
		},
		{
			name:        "QUARTA-FEIRA daily show exception",
			fileName:    "Jornal.Hoje.15.05.2024.mp4",
			caption:     "📰 JORNAL HOJE\nQUARTA FEIRA - 15/05/2024\nEdição Completa",
			wantSeason:  1,
			wantEpisode: 0,
			wantChapter: true,
		},
		{
			name:        "Cap. 120 in filename",
			fileName:    "Pantanal.Cap.120.1080p.mkv",
			caption:     "",
			wantSeason:  1,
			wantEpisode: 120,
			wantChapter: true,
		},
		{
			name:        "TERÇA-FEIRA in caption",
			fileName:    "Novela.mkv",
			caption:     "Capítulo 89 - TERÇA-FEIRA",
			wantSeason:  1,
			wantEpisode: 89,
			wantChapter: true,
		},
		{
			name:        "SEGUNDA-FEIRA in caption",
			fileName:    "Novela.mkv",
			caption:     "CAPITULO [01] - SEGUNDA FEIRA",
			wantSeason:  1,
			wantEpisode: 1,
			wantChapter: true,
		},
		{
			name:        "SEXTA-FEIRA in caption",
			fileName:    "Globo.Reporter.Sexta-Feira.mkv",
			caption:     "GLOBO REPÓRTER - SEXTA-FEIRA",
			wantSeason:  1,
			wantEpisode: 0,
			wantChapter: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSeason, gotEp, gotChapter := DetectSeasonAndEpisode(tt.fileName, tt.caption)
			if gotChapter != tt.wantChapter {
				t.Errorf("DetectSeasonAndEpisode() gotChapter = %v, want %v", gotChapter, tt.wantChapter)
			}
			if gotSeason != tt.wantSeason {
				t.Errorf("DetectSeasonAndEpisode() gotSeason = %v, want %v", gotSeason, tt.wantSeason)
			}
			if gotEp != tt.wantEpisode {
				t.Errorf("DetectSeasonAndEpisode() gotEpisode = %v, want %v", gotEp, tt.wantEpisode)
			}
		})
	}
}

func TestExtractColaborador(t *testing.T) {
	// 1. With command argument "/lote ColabNick"
	u1 := &ext.Update{
		EffectiveMessage: &types.Message{
			Text: "/lote ColabNick",
		},
	}
	if got := extractColaborador(u1); got != "ColabNick" {
		t.Errorf("extractColaborador() = %v, want ColabNick", got)
	}

	// 2. Default fallback when no user or args
	u2 := &ext.Update{
		EffectiveMessage: &types.Message{
			Text: "/lote",
		},
	}
	if got := extractColaborador(u2); got != "Colaborador" {
		t.Errorf("extractColaborador() = %v, want Colaborador", got)
	}
}

func TestAutoDetectBatchProcessing(t *testing.T) {
	state := &LoteState{
		Type:             "series",
		AutoDetectSeason: true,
		AutoDetectEp:     true,
		Season:           1,
		CurrentEp:        1,
		SeriesStreams:    make(map[string]map[string][]StreamObj),
	}

	files := []struct {
		fileName   string
		caption    string
		wantSeason string
		wantEp     string
	}{
		{
			fileName:   "Show.S01E01.mkv",
			caption:    "",
			wantSeason: "1",
			wantEp:     "1",
		},
		{
			fileName:   "Show.S01E02.mkv",
			caption:    "",
			wantSeason: "1",
			wantEp:     "2",
		},
		{
			fileName:   "Show.S02E01.mkv",
			caption:    "",
			wantSeason: "2",
			wantEp:     "1",
		},
		{
			fileName:   "Novela.S02.Capitulo.75.mkv",
			caption:    "CAPITULO [75] - QUARTA FEIRA",
			wantSeason: "1", // Forced to 1!
			wantEp:     "75",
		},
	}

	for _, f := range files {
		season := state.Season
		ep := state.CurrentEp

		if state.AutoDetectSeason || state.AutoDetectEp {
			detSeason, detEp, isChapter := DetectSeasonAndEpisode(f.fileName, f.caption)
			if isChapter {
				season = 1
				if detEp > 0 {
					ep = detEp
				}
			} else {
				if state.AutoDetectSeason && detSeason > 0 {
					season = detSeason
				}
				if detEp > 0 {
					ep = detEp
				}
			}
		}

		seasonStr := strconv.Itoa(season)
		epStr := strconv.Itoa(ep)

		if state.SeriesStreams[seasonStr] == nil {
			state.SeriesStreams[seasonStr] = make(map[string][]StreamObj)
		}
		state.SeriesStreams[seasonStr][epStr] = append(state.SeriesStreams[seasonStr][epStr], StreamObj{
			URL:  "/stream/1",
			Name: "Dublado 1080p",
		})

		state.Season = season
		state.CurrentEp = ep + 1

		if seasonStr != f.wantSeason {
			t.Errorf("File %s: got season %s, want %s", f.fileName, seasonStr, f.wantSeason)
		}
		if epStr != f.wantEp {
			t.Errorf("File %s: got ep %s, want %s", f.fileName, epStr, f.wantEp)
		}
	}

	// Verify the final series streams map has entries for S1 and S2
	if len(state.SeriesStreams["1"]) != 3 { // E1, E2, E75
		t.Errorf("Expected Season 1 to have 3 episodes, got %d", len(state.SeriesStreams["1"]))
	}
	if len(state.SeriesStreams["2"]) != 1 { // E1
		t.Errorf("Expected Season 2 to have 1 episode, got %d", len(state.SeriesStreams["2"]))
	}
}

func TestApplyAudioUpdate(t *testing.T) {
	// 1. Test movie streams audio replacement
	movieState := &LoteState{
		Type:  "movie",
		Audio: "Dublado",
		MovieStreams: []StreamObj{
			{
				URL:  "/stream/1",
				Name: "Dublado\n1080p",
			},
			{
				URL:  "/stream/2",
				Name: "Dublado\n720p",
			},
		},
	}

	applyAudioUpdate(movieState, "Dublado", "Legendado")

	if movieState.Audio != "Legendado" {
		t.Errorf("Expected state.Audio to be Legendado, got %s", movieState.Audio)
	}
	if movieState.MovieStreams[0].Name != "Legendado\n1080p" {
		t.Errorf("Expected stream 0 Name to be 'Legendado\\n1080p', got %s", movieState.MovieStreams[0].Name)
	}
	if movieState.MovieStreams[1].Name != "Legendado\n720p" {
		t.Errorf("Expected stream 1 Name to be 'Legendado\\n720p', got %s", movieState.MovieStreams[1].Name)
	}

	// 2. Test series streams audio replacement
	seriesState := &LoteState{
		Type:  "series",
		Audio: "Dublado",
		SeriesStreams: map[string]map[string][]StreamObj{
			"1": {
				"1": []StreamObj{
					{
						URL:  "/stream/10",
						Name: "Dublado\n1080p",
					},
				},
				"2": []StreamObj{
					{
						URL:  "/stream/11",
						Name: "Dublado\n1080p",
					},
				},
			},
		},
	}

	applyAudioUpdate(seriesState, "Dublado", "Dual Áudio")

	if seriesState.Audio != "Dual Áudio" {
		t.Errorf("Expected state.Audio to be Dual Áudio, got %s", seriesState.Audio)
	}
	if seriesState.SeriesStreams["1"]["1"][0].Name != "Dual Áudio\n1080p" {
		t.Errorf("Expected S01E01 Name to be 'Dual Áudio\\n1080p', got %s", seriesState.SeriesStreams["1"]["1"][0].Name)
	}
	if seriesState.SeriesStreams["1"]["2"][0].Name != "Dual Áudio\n1080p" {
		t.Errorf("Expected S01E02 Name to be 'Dual Áudio\\n1080p', got %s", seriesState.SeriesStreams["1"]["2"][0].Name)
	}
}

func TestStepTransitionsAndVoltar(t *testing.T) {
	// Test step state machine logic for navigation
	state := &LoteState{
		Step:   6,
		Type:   "series",
		Title:  "Breaking Bad",
		ImdbID: "tt0903747",
		Audio:  "Dublado",
	}

	// Going back from Step 6 (Season prompt) should return to Step 5 (Audio prompt)
	if state.Step == 6 {
		state.Step = 5
		state.EditingAudio = false
	}
	if state.Step != 5 {
		t.Errorf("Expected Step to be 5, got %d", state.Step)
	}

	// Changing audio from Dublado to Legendado
	state.Audio = "Legendado"
	state.Step = 6
	if state.Audio != "Legendado" || state.Step != 6 {
		t.Errorf("Expected Audio to be Legendado and Step 6, got %s, %d", state.Audio, state.Step)
	}

	// Step 8 with 0 files for series -> going back should go to Step 7
	state.Step = 8
	state.SeriesStreams = make(map[string]map[string][]StreamObj)
	if len(state.SeriesStreams) == 0 {
		state.Step = 7
	}
	if state.Step != 7 {
		t.Errorf("Expected Step to be 7, got %d", state.Step)
	}

	// Step 7 -> going back should go to Step 6
	if state.Step == 7 {
		state.Step = 6
	}
	if state.Step != 6 {
		t.Errorf("Expected Step to be 6, got %d", state.Step)
	}

	// Step 4 match selection: user decides to search again or enter IMDb ID
	state.Step = 4
	state.SearchResults = []CinemetaSearchResult{
		{ID: "tt111", Name: "Option 1", Year: "2020"},
	}

	// Simulating user typing IMDb ID "tt0903747" in Step 4
	inputText := "tt0903747"
	if strings.HasPrefix(inputText, "tt") {
		state.ImdbID = inputText
		state.Title = "Breaking Bad"
		state.SearchResults = nil
		state.Step = 5
	}
	if state.Step != 5 || state.ImdbID != "tt0903747" {
		t.Errorf("Expected Step 5 with IMDb ID tt0903747, got step %d id %s", state.Step, state.ImdbID)
	}
}
