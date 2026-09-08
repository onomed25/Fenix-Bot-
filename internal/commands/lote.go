package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/dispatcher/handlers/filters"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

type StreamObj struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	Colaborador string `json:"colaborador,omitempty"`
}

type CinemetaSearchResult struct {
	ID   string
	Name string
	Year string
}

type CinemetaVideo struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Number  int    `json:"number"`
	Title   string `json:"name"`
}

type LoteState struct {
	Step              int // 2: Type, 3: Search, 4: MatchSelection, 5: Audio, 6: Season, 7: StartEp, 8: Files
	Colaborador       string
	Type              string // "movie" or "series"
	ImdbID            string
	Title             string
	Audio             string
	EditingAudio      bool
	Season            int
	CurrentEp         int
	IsAbsoluteEp      bool
	CurrentAbsoluteEp int
	SeriesVideos      []CinemetaVideo
	AutoDetectSeason  bool
	AutoDetectEp      bool
	MovieStreams      []StreamObj
	SeriesStreams     map[string]map[string][]StreamObj // Season -> Ep -> []StreamObj
	SearchResults     []CinemetaSearchResult
	Mutex             sync.Mutex // Per-user lock for processing files & messages
}

var (
	// Chapter / Daily markers: "CAPITULO [X]", "QUARTA FEIRA", etc.
	chapterRegex = regexp.MustCompile(`(?i)(` +
		`\bcap[ií]tulos?\b` +
		`|\bcap\.?\s*\[?\s*\d+\s*\]?` +
		`|\b(segunda|ter[cç]a|quarta|quinta|sexta)[\s\-_]*feiras?\b` +
		`|\b(edi[cç][aã]o\s+de\s+)?(s[aá]bado|domingo)\s*[-–—/:]?\s*\d{1,2}` +
		`|\b(novela|programa\s+di[aá]rio)\b` +
		`)`)

	chapterNumRegex        = regexp.MustCompile(`(?i)\bcap(?:[ií]tulo|\.)?\s*\[?\s*(\d+)\s*\]?`)
	chapterNumOrdinalRegex = regexp.MustCompile(`(?i)\b(\d{1,4})\s*º?\s*cap[ií]tulo\b`)

	// Series patterns
	sePatternRegex        = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])[ST](\d{1,2})[\.\-\_\s]*(?:E|EP)[\.\-\_\s]*(\d{1,3})(?:[^a-zA-Z0-9]|$)`)
	xPatternRegex         = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(\d{1,2})x(\d{1,3})(?:[^a-zA-Z0-9]|$)`)
	seasonOrdinalRegex    = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(\d{1,2})\s*(?:[ªºa]|ª|º)?[\s\.\-_]*temporada(?:[^a-zA-Z0-9]|$)`)
	seasonWordRegex       = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(?:temporada|season|temp\.?)[\s\.\-_:]*S?(\d{1,2})(?:[^a-zA-Z0-9]|$)`)
	seasonStandaloneRegex = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])[ST](\d{1,2})(?:[^a-zA-Z0-9]|$)`)
	epWordRegex           = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(?:epis[oó]dio|episode|ep\.?)[\s\.\-_:]*E?(\d{1,4})(?:[^a-zA-Z0-9]|$)`)
	epStandaloneRegex     = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])(?:E|EP)[\.\-\_\s]*(\d{1,4})(?:[^a-zA-Z0-9]|$)`)

	// Absolute episode detection patterns
	absEpExplicitRegex   = regexp.MustCompile(`(?i)\b(?:epis[oó]dio|episode|ep\.?)\s*E?(\d{1,4})\b`)
	absEpStandaloneRegex = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9])E(\d{1,4})(?:[^a-zA-Z0-9]|$)`)
	absEpDashRegex       = regexp.MustCompile(`(?:^|[\s\.\-])-\s*(\d{1,4})(?:[\s\.\-\]]|$)`)
	absEpPureNumRegex    = regexp.MustCompile(`^\s*(\d{1,4})\s*$`)
	absEpTrailingRegex   = regexp.MustCompile(`(?:^|[\s\._])(\d{1,4})(?:\s*\[|\s*\(|\s*$)`)
	cleanTagsRegex       = regexp.MustCompile(`(?i)\b(?:2160p|1080p|720p|480p|360p|4k|2k|8k|x264|x265|h264|h265|hevc|10bit|8bit|aac|flac|mp3|dvdrip|bluray|web-dl|webrip)\b`)
	extRegex             = regexp.MustCompile(`\.[a-zA-Z0-9]{2,5}$`)
)

func extractColaborador(u *ext.Update) string {
	if u.EffectiveMessage != nil {
		text := strings.TrimSpace(u.EffectiveMessage.Text)
		if strings.HasPrefix(text, "/lote") {
			arg := strings.TrimSpace(strings.TrimPrefix(text, "/lote"))
			if arg != "" {
				return arg
			}
		}
	}
	if user := u.EffectiveUser(); user != nil {
		if user.Username != "" {
			return user.Username
		}
		fullName := strings.TrimSpace(user.FirstName + " " + user.LastName)
		if fullName != "" {
			return fullName
		}
		if user.FirstName != "" {
			return user.FirstName
		}
		if user.ID != 0 {
			return strconv.FormatInt(user.ID, 10)
		}
	}
	return "Colaborador"
}

func isChapterContent(text string) bool {
	return chapterRegex.MatchString(text)
}

func extractChapterNumber(text string) int {
	if m := chapterNumRegex.FindStringSubmatch(text); len(m) > 1 {
		if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
			return val
		}
	}
	if m := chapterNumOrdinalRegex.FindStringSubmatch(text); len(m) > 1 {
		if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
			return val
		}
	}
	return 0
}

func DetectSeasonAndEpisode(fileName, caption string) (season int, episode int, isChapter bool) {
	combined := fileName + "\n" + caption
	if isChapterContent(combined) {
		isChapter = true
		season = 1 // Forced to Season 1 for chapter releases, ignoring any multiple seasons
		episode = extractChapterNumber(combined)
		return season, episode, isChapter
	}

	// 1. Try SxxExx or TxxExx in fileName then caption
	if m := sePatternRegex.FindStringSubmatch(fileName); len(m) > 2 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		return s, e, false
	}
	if m := sePatternRegex.FindStringSubmatch(caption); len(m) > 2 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		return s, e, false
	}

	// 2. Try (\d+)x(\d+) in fileName then caption
	if m := xPatternRegex.FindStringSubmatch(fileName); len(m) > 2 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		if !(s == 16 && e == 9) && !(s == 4 && e == 3) {
			return s, e, false
		}
	}
	if m := xPatternRegex.FindStringSubmatch(caption); len(m) > 2 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		if !(s == 16 && e == 9) && !(s == 4 && e == 3) {
			return s, e, false
		}
	}

	// 3. Try Season words in fileName then caption
	searchSeason := func(t string) int {
		if m := seasonOrdinalRegex.FindStringSubmatch(t); len(m) > 1 {
			if s, err := strconv.Atoi(m[1]); err == nil && s > 0 {
				return s
			}
		}
		if m := seasonWordRegex.FindStringSubmatch(t); len(m) > 1 {
			if s, err := strconv.Atoi(m[1]); err == nil && s > 0 {
				return s
			}
		}
		if m := seasonStandaloneRegex.FindStringSubmatch(t); len(m) > 1 {
			if s, err := strconv.Atoi(m[1]); err == nil && s > 0 {
				return s
			}
		}
		return 0
	}

	if s := searchSeason(fileName); s > 0 {
		season = s
	} else if s := searchSeason(caption); s > 0 {
		season = s
	}

	// 4. Try Episode words in fileName then caption
	searchEp := func(t string) int {
		if m := epWordRegex.FindStringSubmatch(t); len(m) > 1 {
			if e, err := strconv.Atoi(m[1]); err == nil && e > 0 {
				return e
			}
		}
		if m := epStandaloneRegex.FindStringSubmatch(t); len(m) > 1 {
			if e, err := strconv.Atoi(m[1]); err == nil && e > 0 {
				return e
			}
		}
		return 0
	}

	if e := searchEp(fileName); e > 0 {
		episode = e
	} else if e := searchEp(caption); e > 0 {
		episode = e
	}

	return season, episode, false
}

func DetectAbsoluteEpisode(fileName, caption string) int {
	search := func(text string) int {
		if text == "" {
			return 0
		}
		// Strip extension
		cleaned := extRegex.ReplaceAllString(text, "")
		// Replace underscores with spaces
		cleaned = strings.ReplaceAll(cleaned, "_", " ")
		// Remove resolution and codec tags
		cleaned = cleanTagsRegex.ReplaceAllString(cleaned, " ")

		// 1. Explicit markers: "Episodio 33", "Ep 33", "Episode 1085", "ep.50"
		if m := absEpExplicitRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
				return val
			}
		}

		// 2. Standalone E: " E33 "
		if m := absEpStandaloneRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
				return val
			}
		}

		// 3. Anime dash format: " - 033 ", " - 1085 "
		if m := absEpDashRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
				return val
			}
		}

		// 4. Pure number file: "01", "26", "033"
		trimmed := strings.TrimSpace(cleaned)
		if m := absEpPureNumRegex.FindStringSubmatch(trimmed); len(m) > 1 {
			if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
				return val
			}
		}

		// 5. Trailing number before brackets or end: "Naruto Shippuden 033"
		if m := absEpTrailingRegex.FindStringSubmatch(cleaned); len(m) > 1 {
			if val, err := strconv.Atoi(m[1]); err == nil && val > 0 {
				if val < 1990 || val > 2029 {
					return val
				}
			}
		}

		return 0
	}

	if e := search(fileName); e > 0 {
		return e
	}
	if e := search(caption); e > 0 {
		return e
	}
	return 0
}

func (s *LoteState) MapAbsoluteToSeasonEp(absEp int) (season, ep int, title string) {
	if len(s.SeriesVideos) == 0 {
		if absEp < 1 {
			absEp = 1
		}
		return 1, absEp, ""
	}
	if absEp < 1 {
		absEp = 1
	}
	if absEp <= len(s.SeriesVideos) {
		v := s.SeriesVideos[absEp-1]
		return v.Season, v.Episode, v.Title
	}
	// Beyond known episodes in IMDb (e.g. newly airing): append to last season
	last := s.SeriesVideos[len(s.SeriesVideos)-1]
	diff := absEp - len(s.SeriesVideos)
	return last.Season, last.Episode + diff, ""
}

func (s *LoteState) MapSeasonEpToAbsolute(season, ep int) int {
	for i, v := range s.SeriesVideos {
		if v.Season == season && v.Episode == ep {
			return i + 1
		}
	}
	if season == 1 {
		return ep
	}
	return 0
}

func getSearchBackMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔙 Voltar (Tipo)",
						Data: []byte("lote_back_to_type"),
					},
					&tg.KeyboardButtonCallback{
						Text: "❌ Cancelar",
						Data: []byte("lote_cancelar"),
					},
				},
			},
		},
	}
}

func sendSearchPrompt(ctx *ext.Context, u *ext.Update, contentType string) error {
	typeName := "Série / Anime"
	if contentType == "movie" {
		typeName = "Filme"
	}
	return sendLoteResponse(ctx, u, fmt.Sprintf("Tipo configurado: **%s**.\n\nAgora digite o Nome para buscar ou envie o ID IMDb (começando com 'tt', ex: `tt1234567`):", typeName), getSearchBackMarkup())
}

func sendMatchSelectionPrompt(ctx *ext.Context, u *ext.Update, results []CinemetaSearchResult) error {
	rows := []tg.KeyboardButtonRow{}
	for _, r := range results {
		rows = append(rows, tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{
					Text: fmt.Sprintf("%s (%s)", r.Name, r.Year),
					Data: []byte("lote_sel_" + r.ID),
				},
			},
		})
	}
	rows = append(rows, tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{
				Text: "🔍 Digitar ID IMDb / Nova Busca",
				Data: []byte("lote_back_to_search"),
			},
		},
	})
	rows = append(rows, tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{
				Text: "🔙 Voltar",
				Data: []byte("lote_back_to_type"),
			},
			&tg.KeyboardButtonCallback{
				Text: "❌ Cancelar",
				Data: []byte("lote_cancelar"),
			},
		},
	})
	return sendLoteResponse(ctx, u, "Escolha o item correto clicando em um dos botões abaixo, ou digite o ID IMDb (ex: `tt1234567`) ou um novo nome:", &tg.ReplyInlineMarkup{Rows: rows})
}

func getSkipSeasonMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔢 Episódios Absolutos",
						Data: []byte("lote_absolute_ep"),
					},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "⏩ Deixar em branco (Auto-detectar)",
						Data: []byte("lote_skip_season"),
					},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔙 Voltar (Alterar Áudio)",
						Data: []byte("lote_back_to_audio"),
					},
					&tg.KeyboardButtonCallback{
						Text: "❌ Cancelar",
						Data: []byte("lote_cancelar"),
					},
				},
			},
		},
	}
}

func sendSeasonPrompt(ctx *ext.Context, u *ext.Update) {
	sendLoteResponse(ctx, u, "Qual é a temporada do lote?\n\n• Digite o número da temporada (Ex: `1`)\n• Clique em **⏩ Deixar em branco** para detecção automática\n• Ou clique em **🔢 Episódios Absolutos** se a série/anime usa numeração contínua sem temporadas no arquivo (o bot converterá para as temporadas do IMDb):", getSkipSeasonMarkup())
}

func getSkipAbsoluteEpMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "⏩ Deixar em branco (Auto-detectar)",
						Data: []byte("lote_skip_abs_ep"),
					},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔙 Voltar (Temporada)",
						Data: []byte("lote_back_to_season"),
					},
					&tg.KeyboardButtonCallback{
						Text: "❌ Cancelar",
						Data: []byte("lote_cancelar"),
					},
				},
			},
		},
	}
}

func sendAbsoluteEpPrompt(ctx *ext.Context, u *ext.Update, state *LoteState, numSeasons, totalEps int) error {
	var info string
	if numSeasons > 0 && totalEps > 0 {
		info = fmt.Sprintf("🔢 **Modo Episódios Absolutos ativado!**\n- **Série**: %s (`%s`)\n- **Estrutura IMDb**: **%d temporada(s)** e **%d episódios** cadastrados.\n\nQual é o **número do episódio absoluto inicial**? (Ex: `1` para começar do ep 1, `33` para começar no 33, ou deixe em branco para detecção automática):", state.Title, state.ImdbID, numSeasons, totalEps)
	} else {
		info = fmt.Sprintf("🔢 **Modo Episódios Absolutos ativado!**\n- **Série**: %s (`%s`)\n\nQual é o **número do episódio absoluto inicial**? (Ex: `1` ou deixe em branco para detecção automática):", state.Title, state.ImdbID)
	}
	return sendLoteResponse(ctx, u, info, getSkipAbsoluteEpMarkup())
}

func getSkipEpMarkup() *tg.ReplyInlineMarkup {
	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "⏩ Deixar em branco (Auto-detectar)",
						Data: []byte("lote_skip_ep"),
					},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "🔙 Voltar (Temporada)",
						Data: []byte("lote_back_to_season"),
					},
					&tg.KeyboardButtonCallback{
						Text: "❌ Cancelar",
						Data: []byte("lote_cancelar"),
					},
				},
			},
		},
	}
}

func sendEpPrompt(ctx *ext.Context, u *ext.Update) {
	sendLoteResponse(ctx, u, "Qual é o número do episódio inicial? (Ex: 1 ou deixe em branco para detecção automática):", getSkipEpMarkup())
}

var (
	loteStates = make(map[int64]*LoteState)
	loteMutex  sync.Mutex
)

func sendLoteResponse(ctx *ext.Context, u *ext.Update, text string, markup tg.ReplyMarkupClass) error {
	if u.CallbackQuery != nil {
		chatId := u.EffectiveChat().GetID()
		peer := ctx.PeerStorage.GetInputPeerById(chatId)
		if peer.Zero() {
			peer = &tg.InputPeerUser{UserID: chatId}
		}
		if markup != nil {
			_, err := ctx.Sender.To(peer).Markup(markup).Text(ctx, text)
			return err
		} else {
			_, err := ctx.Sender.To(peer).Text(ctx, text)
			return err
		}
	}

	var opts *ext.ReplyOpts
	if markup != nil {
		opts = &ext.ReplyOpts{
			Markup: markup,
		}
	}
	_, err := ctx.Reply(u, ext.ReplyTextString(text), opts)
	return err
}

func applyAudioUpdate(state *LoteState, oldAudio, newAudio string) {
	state.Audio = newAudio
	if state.Type == "movie" {
		for i := range state.MovieStreams {
			if oldAudio != "" && strings.Contains(state.MovieStreams[i].Name, oldAudio) {
				state.MovieStreams[i].Name = strings.Replace(state.MovieStreams[i].Name, oldAudio, newAudio, 1)
			} else {
				parts := strings.Split(state.MovieStreams[i].Name, "\n")
				if len(parts) > 1 {
					state.MovieStreams[i].Name = fmt.Sprintf("%s\n%s", newAudio, strings.Join(parts[1:], "\n"))
				} else {
					state.MovieStreams[i].Name = newAudio
				}
			}
		}
	} else if state.Type == "series" && state.SeriesStreams != nil {
		for seasonKey, epMap := range state.SeriesStreams {
			for epKey, streams := range epMap {
				for i := range streams {
					if oldAudio != "" && strings.Contains(streams[i].Name, oldAudio) {
						streams[i].Name = strings.Replace(streams[i].Name, oldAudio, newAudio, 1)
					} else {
						parts := strings.Split(streams[i].Name, "\n")
						if len(parts) > 1 {
							streams[i].Name = fmt.Sprintf("%s\n%s", newAudio, strings.Join(parts[1:], "\n"))
						} else {
							streams[i].Name = newAudio
						}
					}
				}
				state.SeriesStreams[seasonKey][epKey] = streams
			}
		}
	}
}

func handleVoltar(ctx *ext.Context, u *ext.Update, state *LoteState) {
	switch state.Step {
	case 2:
		sendLoteResponse(ctx, u, "Você já está no início da configuração. Escolha o tipo de conteúdo ou use `/cancelar` para sair.", nil)
	case 3:
		state.Step = 2
		sendTypePrompt(ctx, u)
	case 4:
		state.Step = 3
		sendSearchPrompt(ctx, u, state.Type)
	case 5:
		if state.EditingAudio {
			state.EditingAudio = false
			state.Step = 8
			sendLoteResponse(ctx, u, fmt.Sprintf("Alteração cancelada. Áudio mantido: **%s**.\n\nEnvie os arquivos de vídeo ou clique em **Concluir Lote**.", state.Audio), getWaitingFilesMarkup(state))
			return
		}
		if len(state.SearchResults) > 0 {
			state.Step = 4
			sendMatchSelectionPrompt(ctx, u, state.SearchResults)
		} else {
			state.Step = 3
			sendSearchPrompt(ctx, u, state.Type)
		}
	case 6:
		state.Step = 5
		state.EditingAudio = false
		sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
	case 7:
		if state.IsAbsoluteEp {
			state.IsAbsoluteEp = false
		}
		state.Step = 6
		sendSeasonPrompt(ctx, u)
	case 8:
		hasFiles := false
		if state.Type == "movie" && len(state.MovieStreams) > 0 {
			hasFiles = true
		} else if state.Type == "series" && len(state.SeriesStreams) > 0 {
			hasFiles = true
		}

		if hasFiles {
			sendLoteResponse(ctx, u, "Já existem arquivos adicionados neste lote.\n\n- Para alterar o áudio, clique no botão **🔊 Alterar Áudio** ou envie `/audio`.\n- Para alterar temporada/episódio, use `/temporada <N>` ou `/episodio <N>`.\n- Para cancelar o lote, envie `/cancelar`.", getWaitingFilesMarkup(state))
		} else {
			if state.Type == "series" {
				state.Step = 7
				if state.IsAbsoluteEp {
					uniqueSeasons := make(map[int]bool)
					for _, v := range state.SeriesVideos {
						uniqueSeasons[v.Season] = true
					}
					sendAbsoluteEpPrompt(ctx, u, state, len(uniqueSeasons), len(state.SeriesVideos))
				} else {
					sendEpPrompt(ctx, u)
				}
			} else {
				state.Step = 5
				state.EditingAudio = false
				sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
			}
		}
	}
}

func getWaitingFilesMarkup(state *LoteState) tg.ReplyMarkupClass {
	if state.Type == "movie" {
		bottomRow := tg.KeyboardButtonRow{}
		if len(state.MovieStreams) == 0 {
			bottomRow.Buttons = append(bottomRow.Buttons, &tg.KeyboardButtonCallback{Text: "🔙 Voltar", Data: []byte("lote_back_from_files")})
		}
		bottomRow.Buttons = append(bottomRow.Buttons, &tg.KeyboardButtonCallback{Text: "❌ Cancelar", Data: []byte("lote_cancelar")})

		return &tg.ReplyInlineMarkup{
			Rows: []tg.KeyboardButtonRow{
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "🔊 Alterar Áudio", Data: []byte("lote_change_audio")},
						&tg.KeyboardButtonCallback{Text: "📦 Concluir Lote", Data: []byte("lote_concluir")},
					},
				},
				bottomRow,
			},
		}
	}

	// Series/Anime markup
	var row1 tg.KeyboardButtonRow
	var row2 tg.KeyboardButtonRow

	if state.IsAbsoluteEp {
		absNext := state.CurrentAbsoluteEp
		sNext, eNext, _ := state.MapAbsoluteToSeasonEp(absNext)
		row1 = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: fmt.Sprintf("➕ Ep Abs (#%d)", absNext+1), Data: []byte("lote_inc_abs_ep")},
			},
		}
		if absNext > 1 {
			row1.Buttons = append(row1.Buttons, &tg.KeyboardButtonCallback{Text: fmt.Sprintf("➖ Ep Abs (#%d)", absNext-1), Data: []byte("lote_dec_abs_ep")})
		}

		row2 = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: fmt.Sprintf("🔢 #%d ➔ S%02dE%02d", absNext, sNext, eNext), Data: []byte("lote_info_abs")},
			},
		}
	} else {
		row1 = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: fmt.Sprintf("➕ Temp (S%d)", state.Season+1), Data: []byte("lote_inc_season")},
			},
		}
		if state.Season > 1 {
			row1.Buttons = append(row1.Buttons, &tg.KeyboardButtonCallback{Text: fmt.Sprintf("➖ Temp (S%d)", state.Season-1), Data: []byte("lote_dec_season")})
		}

		row2 = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: fmt.Sprintf("➕ Ep (E%d)", state.CurrentEp+1), Data: []byte("lote_inc_ep")},
			},
		}
		if state.CurrentEp > 1 {
			row2.Buttons = append(row2.Buttons, &tg.KeyboardButtonCallback{Text: fmt.Sprintf("➖ Ep (E%d)", state.CurrentEp-1), Data: []byte("lote_dec_ep")})
		}
	}

	row3 := tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🔊 Alterar Áudio", Data: []byte("lote_change_audio")},
			&tg.KeyboardButtonCallback{Text: "📦 Concluir Lote", Data: []byte("lote_concluir")},
		},
	}

	row4 := tg.KeyboardButtonRow{}
	if len(state.SeriesStreams) == 0 {
		row4.Buttons = append(row4.Buttons, &tg.KeyboardButtonCallback{Text: "🔙 Voltar", Data: []byte("lote_back_from_files")})
	}
	row4.Buttons = append(row4.Buttons, &tg.KeyboardButtonCallback{Text: "❌ Cancelar", Data: []byte("lote_cancelar")})

	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			row1,
			row2,
			row3,
			row4,
		},
	}
}

func (m *command) LoadLote(dispatcher dispatcher.Dispatcher) {
	log := m.log.Named("lote")
	defer log.Sugar().Info("Loaded")
	dispatcher.AddHandler(handlers.NewCommand("lote", startLote))
	dispatcher.AddHandler(handlers.NewCommand("concluido", concluirLote))
	dispatcher.AddHandler(handlers.NewCommand("cancelar", cancelarLote))
	dispatcher.AddHandler(handlers.NewCommand("voltar", voltarLote))
	dispatcher.AddHandler(handlers.NewCommand("audio", audioLote))
	dispatcher.AddHandler(handlers.NewCallbackQuery(filters.CallbackQuery.Prefix("lote_"), handleLoteCallbackQuery))
}

func HandleLoteMessage(ctx *ext.Context, u *ext.Update) (bool, error) {
	if u.EffectiveMessage != nil && u.EffectiveMessage.Out {
		return false, nil
	}

	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return false, nil
	}

	loteMutex.Lock()
	state, exists := loteStates[chatId]
	loteMutex.Unlock()

	if !exists {
		return false, nil
	}

	state.Mutex.Lock()
	defer state.Mutex.Unlock()

	text := ""
	if u.EffectiveMessage != nil && u.EffectiveMessage.Text != "" {
		text = strings.TrimSpace(u.EffectiveMessage.Text)
	}

	// Commands and text shortcuts
	if strings.EqualFold(text, "voltar") || text == "/voltar" {
		handleVoltar(ctx, u, state)
		return true, nil
	}

	if strings.EqualFold(text, "cancelar") {
		cancelarLoteHelper(ctx, u, chatId)
		return true, nil
	}

	if (strings.EqualFold(text, "audio") || text == "/audio") && state.Step >= 5 {
		state.EditingAudio = true
		state.Step = 5
		sendAudioPrompt(ctx, u, state.Title, state.ImdbID, true)
		return true, nil
	}

	// If it's a command, let other handlers process it
	if strings.HasPrefix(text, "/") {
		if state.Step == 8 && (strings.HasPrefix(text, "/temporada ") || strings.HasPrefix(text, "/episodio ") || strings.HasPrefix(text, "/absoluto") || strings.HasPrefix(text, "/ep ")) {
			if state.Type != "series" {
				sendLoteResponse(ctx, u, "Esta opção só é válida para Séries/Animes.", nil)
				return true, nil
			}
			if strings.HasPrefix(text, "/absoluto") || strings.HasPrefix(text, "/ep ") {
				valStr := ""
				if strings.HasPrefix(text, "/absoluto ") {
					valStr = strings.TrimPrefix(text, "/absoluto ")
				} else if strings.HasPrefix(text, "/ep ") {
					valStr = strings.TrimPrefix(text, "/ep ")
				} else if text == "/absoluto" {
					if !state.IsAbsoluteEp {
						activateAbsoluteEpMode(ctx, u, state)
						return true, nil
					}
					sNext, eNext, _ := state.MapAbsoluteToSeasonEp(state.CurrentAbsoluteEp)
					sendLoteResponse(ctx, u, fmt.Sprintf("🔢 Modo Episódios Absolutos ativo.\nPróximo esperado: **#%d** (Corresponde a **S%02dE%02d** no IMDb).", state.CurrentAbsoluteEp, sNext, eNext), getWaitingFilesMarkup(state))
					return true, nil
				}
				valStr = strings.TrimSpace(valStr)
				absNum, err := strconv.Atoi(valStr)
				if err != nil || absNum < 1 {
					sendLoteResponse(ctx, u, "Número de episódio absoluto inválido. Digite um número positivo: (Ex: `/absoluto 26`)", nil)
					return true, nil
				}
				if len(state.SeriesVideos) == 0 {
					videos, _ := fetchCinemetaSeriesVideos(state.ImdbID)
					state.SeriesVideos = videos
				}
				state.IsAbsoluteEp = true
				state.CurrentAbsoluteEp = absNum
				sNext, eNext, _ := state.MapAbsoluteToSeasonEp(absNum)
				state.Season = sNext
				state.CurrentEp = eNext
				sendLoteResponse(ctx, u, fmt.Sprintf("🔢 Episódio absoluto esperado alterado para **#%d** (Corresponde a **S%02dE%02d** no IMDb).\nPróximo esperado: **S%02dE%02d**.", absNum, sNext, eNext, sNext, eNext), getWaitingFilesMarkup(state))
				return true, nil
			}
			if strings.HasPrefix(text, "/temporada ") {
				valStr := strings.TrimPrefix(text, "/temporada ")
				season, err := strconv.Atoi(valStr)
				if err != nil || season < 1 {
					sendLoteResponse(ctx, u, "Temporada inválida. Digite um número positivo: (Ex: `/temporada 2`)", nil)
					return true, nil
				}
				state.IsAbsoluteEp = false
				state.Season = season
				state.CurrentEp = 1
				sendLoteResponse(ctx, u, fmt.Sprintf("Modo padrão de temporadas ativado.\nTemporada alterada para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.Season, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
				return true, nil
			}
			if strings.HasPrefix(text, "/episodio ") {
				valStr := strings.TrimPrefix(text, "/episodio ")
				ep, err := strconv.Atoi(valStr)
				if err != nil || ep < 1 {
					sendLoteResponse(ctx, u, "Episódio inválido. Digite um número positivo: (Ex: `/episodio 5`)", nil)
					return true, nil
				}
				if state.IsAbsoluteEp {
					state.CurrentAbsoluteEp = ep
					sNext, eNext, _ := state.MapAbsoluteToSeasonEp(ep)
					state.Season = sNext
					state.CurrentEp = eNext
					sendLoteResponse(ctx, u, fmt.Sprintf("Episódio absoluto atual alterado para **#%d** (Corresponde a **S%02dE%02d** no IMDb).\nPróximo episódio esperado: **S%02dE%02d**.", ep, sNext, eNext, sNext, eNext), getWaitingFilesMarkup(state))
					return true, nil
				}
				state.CurrentEp = ep
				sendLoteResponse(ctx, u, fmt.Sprintf("Episódio atual alterado para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.CurrentEp, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
				return true, nil
			}
		}
		return false, nil
	}

	if state.Step == 6 || state.Step == 7 {
		supported, err := supportedMediaFilter(u.EffectiveMessage)
		if err == nil && supported {
			if state.Step == 6 {
				state.AutoDetectSeason = true
				state.Season = 1
			}
			state.AutoDetectEp = true
			if state.CurrentEp < 1 {
				state.CurrentEp = 1
			}
			if state.SeriesStreams == nil {
				state.SeriesStreams = make(map[string]map[string][]StreamObj)
			}
			state.Step = 8
		}
	}

	switch state.Step {
	case 1: // Legacy Nick fallback
		if text != "" {
			state.Colaborador = text
		}
		state.Step = 2
		sendTypePrompt(ctx, u)
		return true, nil

	case 2: // Type
		if text == "1" {
			state.Type = "series"
			state.Step = 3
			sendSearchPrompt(ctx, u, "series")
		} else if text == "2" {
			state.Type = "movie"
			state.Step = 3
			sendSearchPrompt(ctx, u, "movie")
		} else {
			sendLoteResponse(ctx, u, "Opção inválida. Por favor, responda com:\n1. Série / Anime\n2. Filme", nil)
		}
		return true, nil

	case 3: // Search Query or IMDb ID
		if strings.HasPrefix(text, "tt") {
			title, err := fetchCinemetaDetails(state.Type, text)
			if err != nil {
				sendLoteResponse(ctx, u, "Não encontrei esse ID IMDb no Cinemeta. Digite o nome ou outro ID para buscar novamente:", getSearchBackMarkup())
				return true, nil
			}
			state.ImdbID = text
			state.Title = title
			state.SearchResults = nil
			if state.Type == "series" && state.ImdbID != "" {
				go func(cid string) {
					vids, err := fetchCinemetaSeriesVideos(cid)
					if err == nil && len(vids) > 0 {
						state.Mutex.Lock()
						if state.ImdbID == cid && len(state.SeriesVideos) == 0 {
							state.SeriesVideos = vids
						}
						state.Mutex.Unlock()
					}
				}(state.ImdbID)
			}
			state.Step = 5
			sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
		} else {
			results, err := queryCinemetaCatalog(state.Type, text)
			if err != nil {
				sendLoteResponse(ctx, u, fmt.Sprintf("Erro ao buscar no Cinemeta: %s. Tente novamente ou envie o ID IMDb (começando com 'tt'):", err.Error()), getSearchBackMarkup())
				return true, nil
			}
			if len(results) == 0 {
				sendLoteResponse(ctx, u, "Nenhum resultado encontrado. Digite outro nome ou envie o ID IMDb (começando com 'tt', ex: tt1234567):", getSearchBackMarkup())
				return true, nil
			}
			state.SearchResults = results
			state.Step = 4
			sendMatchSelectionPrompt(ctx, u, results)
		}
		return true, nil

	case 4: // Select match or type IMDb ID / new search
		if strings.HasPrefix(text, "tt") {
			title, err := fetchCinemetaDetails(state.Type, text)
			if err != nil {
				sendLoteResponse(ctx, u, "Não encontrei esse ID IMDb no Cinemeta. Digite outro ID ou nome para buscar novamente:", getSearchBackMarkup())
				return true, nil
			}
			state.ImdbID = text
			state.Title = title
			state.SearchResults = nil
			if state.Type == "series" && state.ImdbID != "" {
				go func(cid string) {
					vids, err := fetchCinemetaSeriesVideos(cid)
					if err == nil && len(vids) > 0 {
						state.Mutex.Lock()
						if state.ImdbID == cid && len(state.SeriesVideos) == 0 {
							state.SeriesVideos = vids
						}
						state.Mutex.Unlock()
					}
				}(state.ImdbID)
			}
			state.Step = 5
			sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
			return true, nil
		}

		idx, err := strconv.Atoi(text)
		if err == nil && idx >= 1 && idx <= len(state.SearchResults) {
			selected := state.SearchResults[idx-1]
			state.ImdbID = selected.ID
			state.Title = selected.Name
			if state.Type == "series" && state.ImdbID != "" {
				go func(cid string) {
					vids, err := fetchCinemetaSeriesVideos(cid)
					if err == nil && len(vids) > 0 {
						state.Mutex.Lock()
						if state.ImdbID == cid && len(state.SeriesVideos) == 0 {
							state.SeriesVideos = vids
						}
						state.Mutex.Unlock()
					}
				}(state.ImdbID)
			}
			state.Step = 5
			sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
			return true, nil
		}

		// User typed a new name query!
		results, err := queryCinemetaCatalog(state.Type, text)
		if err != nil {
			sendLoteResponse(ctx, u, fmt.Sprintf("Erro ao buscar no Cinemeta: %s. Tente novamente ou envie o ID IMDb (começando com 'tt'):", err.Error()), getSearchBackMarkup())
			return true, nil
		}
		if len(results) == 0 {
			sendLoteResponse(ctx, u, "Nenhum resultado encontrado para essa busca. Digite outro nome ou envie o ID IMDb (começando com 'tt', ex: tt1234567):", getSearchBackMarkup())
			return true, nil
		}
		state.SearchResults = results
		sendMatchSelectionPrompt(ctx, u, results)
		return true, nil

	case 5: // Selecting audio
		audioMap := map[string]string{
			"1": "Dublado",
			"2": "Legendado",
			"3": "Dual Áudio",
			"4": "Português (PT-BR)",
			"5": "English",
		}
		audio, ok := audioMap[text]
		if !ok {
			sendLoteResponse(ctx, u, "Opção inválida. Escolha de 1 a 5:\n1. Dublado\n2. Legendado\n3. Dual Áudio\n4. Português (PT-BR)\n5. English", nil)
			return true, nil
		}
		oldAudio := state.Audio
		state.Audio = audio

		if state.EditingAudio {
			state.EditingAudio = false
			state.Step = 8
			applyAudioUpdate(state, oldAudio, state.Audio)
			sendLoteResponse(ctx, u, fmt.Sprintf("🔊 Áudio alterado com sucesso para: **%s**!\n\nEnvie os arquivos de vídeo ou clique em **Concluir Lote**.", state.Audio), getWaitingFilesMarkup(state))
			return true, nil
		}

		if state.Type == "series" {
			state.Step = 6
			sendSeasonPrompt(ctx, u)
		} else {
			if state.MovieStreams == nil {
				state.MovieStreams = []StreamObj{}
			}
			state.Step = 8
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Filme\n- **Título**: %s (%s)\n- **Áudio**: %s\n\nAgora, **envie os arquivos de vídeo** para este lote.\n\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
		}
		return true, nil

	case 6: // Season
		lower := strings.ToLower(text)
		if lower == "absoluto" || lower == "absolutos" || lower == "ep absoluto" || lower == "ep absolutos" || lower == "episodios absolutos" || lower == "episodio absoluto" || lower == "ep abisolutos" {
			activateAbsoluteEpMode(ctx, u, state)
			return true, nil
		}
		if text == "" || lower == "pular" || lower == "skip" || lower == "auto" || lower == "deixar em branco" || lower == "em branco" || text == "-" || text == "0" {
			state.AutoDetectSeason = true
			state.Season = 1
			state.Step = 7
			sendEpPrompt(ctx, u)
			return true, nil
		}
		season, err := strconv.Atoi(text)
		if err != nil || season < 1 {
			sendLoteResponse(ctx, u, "Temporada inválida. Digite um número positivo (ou clique abaixo para detectar automaticamente):", getSkipSeasonMarkup())
			return true, nil
		}
		state.Season = season
		state.AutoDetectSeason = false
		state.Step = 7
		sendEpPrompt(ctx, u)
		return true, nil

	case 7: // Start ep
		if state.IsAbsoluteEp {
			lower := strings.ToLower(text)
			if text == "" || lower == "pular" || lower == "skip" || lower == "auto" || lower == "deixar em branco" || lower == "em branco" || text == "-" || text == "0" {
				state.AutoDetectEp = true
				state.CurrentAbsoluteEp = 1
				sNext, eNext, _ := state.MapAbsoluteToSeasonEp(1)
				state.Season = sNext
				state.CurrentEp = eNext
				state.SeriesStreams = make(map[string]map[string][]StreamObj)
				state.Step = 8
				msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série (Episódios Absolutos)\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Episódio Inicial**: Automático (detectado do arquivo)\n\nAgora, **envie os arquivos de vídeo**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
				sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
				return true, nil
			}
			absNum, err := strconv.Atoi(text)
			if err != nil || absNum < 1 {
				sendLoteResponse(ctx, u, "Episódio absoluto inválido. Digite um número positivo (ou clique abaixo para detectar automaticamente):", getSkipAbsoluteEpMarkup())
				return true, nil
			}
			state.CurrentAbsoluteEp = absNum
			state.AutoDetectEp = false
			sNext, eNext, _ := state.MapAbsoluteToSeasonEp(absNum)
			state.Season = sNext
			state.CurrentEp = eNext
			state.SeriesStreams = make(map[string]map[string][]StreamObj)
			state.Step = 8
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série (Episódios Absolutos)\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Episódio Absoluto Inicial**: #%d (S%02dE%02d no IMDb)\n\nAgora, **envie os arquivos de vídeo em ordem**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio, state.CurrentAbsoluteEp, sNext, eNext)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
			return true, nil
		}

		lower := strings.ToLower(text)
		if text == "" || lower == "pular" || lower == "skip" || lower == "auto" || lower == "deixar em branco" || lower == "em branco" || text == "-" || text == "0" {
			state.AutoDetectEp = true
			state.CurrentEp = 1
			state.SeriesStreams = make(map[string]map[string][]StreamObj)
			state.Step = 8
			seasonDesc := fmt.Sprintf("%d", state.Season)
			if state.AutoDetectSeason {
				seasonDesc = "Automática (pelo arquivo/legenda)"
			}
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Temporada**: %s\n- **Episódio Inicial**: Automático\n\nAgora, **envie os arquivos de vídeo**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio, seasonDesc)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
			return true, nil
		}
		ep, err := strconv.Atoi(text)
		if err != nil || ep < 1 {
			sendLoteResponse(ctx, u, "Episódio inválido. Digite um número positivo (ou clique abaixo para detectar automaticamente):", getSkipEpMarkup())
			return true, nil
		}
		state.CurrentEp = ep
		state.AutoDetectEp = false
		state.SeriesStreams = make(map[string]map[string][]StreamObj)
		state.Step = 8
		seasonDesc := fmt.Sprintf("%d", state.Season)
		if state.AutoDetectSeason {
			seasonDesc = "Automática (pelo arquivo/legenda)"
		}
		msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Temporada**: %s\n- **Episódio Inicial**: %d\n\nAgora, **envie os arquivos de vídeo em ordem**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio, seasonDesc, state.CurrentEp)
		sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
		return true, nil

	case 8: // Waiting for files
		supported, err := supportedMediaFilter(u.EffectiveMessage)
		if err != nil || !supported {
			sendLoteResponse(ctx, u, "Envie um arquivo de vídeo válido ou digite `/concluido` para fechar o lote.", getWaitingFilesMarkup(state))
			return true, nil
		}

		link, fileName, err := getStreamLinkForMessage(ctx, u, chatId)
		if err != nil {
			sendLoteResponse(ctx, u, fmt.Sprintf("Erro ao processar arquivo: %s", err.Error()), getWaitingFilesMarkup(state))
			return true, nil
		}

		quality := detectQuality(u.EffectiveMessage.Media, fileName)
		streamName := fmt.Sprintf("%s\n%s", state.Audio, quality)
		streamObj := StreamObj{
			URL:         link,
			Name:        streamName,
			Colaborador: state.Colaborador,
		}

		if state.Type == "movie" {
			state.MovieStreams = append(state.MovieStreams, streamObj)
			sendLoteResponse(ctx, u, fmt.Sprintf("🎬 Link de filme adicionado!\n- **Nome**: %s\n- **Qualidade**: %s\n- **URL**: %s\n\nEnvie outro arquivo ou clique em **Concluir Lote**.", fileName, quality, link), getWaitingFilesMarkup(state))
		} else if state.IsAbsoluteEp {
			caption := ""
			if u.EffectiveMessage != nil {
				caption = u.EffectiveMessage.Text
			}

			absNum := 0
			// First check if filename has explicit SxxExx
			detSeason, detEp, _ := DetectSeasonAndEpisode(fileName, caption)
			if detSeason > 0 && detEp > 0 {
				mappedAbs := state.MapSeasonEpToAbsolute(detSeason, detEp)
				if mappedAbs > 0 {
					absNum = mappedAbs
				}
			}

			if absNum == 0 {
				if state.AutoDetectEp {
					detAbs := DetectAbsoluteEpisode(fileName, caption)
					if detAbs > 0 {
						absNum = detAbs
					} else {
						absNum = state.CurrentAbsoluteEp
					}
				} else {
					absNum = state.CurrentAbsoluteEp
				}
			}

			if absNum < 1 {
				absNum = 1
			}

			season, ep, epTitle := state.MapAbsoluteToSeasonEp(absNum)

			seasonStr := strconv.Itoa(season)
			epStr := strconv.Itoa(ep)

			if state.SeriesStreams == nil {
				state.SeriesStreams = make(map[string]map[string][]StreamObj)
			}
			if state.SeriesStreams[seasonStr] == nil {
				state.SeriesStreams[seasonStr] = make(map[string][]StreamObj)
			}
			state.SeriesStreams[seasonStr][epStr] = append(state.SeriesStreams[seasonStr][epStr], streamObj)

			state.CurrentAbsoluteEp = absNum + 1
			nextAbs := state.CurrentAbsoluteEp
			nextS, nextE, _ := state.MapAbsoluteToSeasonEp(nextAbs)
			state.Season = nextS
			state.CurrentEp = nextE

			titleLine := ""
			if epTitle != "" {
				titleLine = fmt.Sprintf("\n- **Título IMDb**: %s", epTitle)
			}

			sendLoteResponse(ctx, u, fmt.Sprintf("📺 **S%02dE%02d** (Ep. Absoluto #%d) adicionado!\n- **Temporada**: %d\n- **Episódio**: %d\n- **Ep. Absoluto**: %d%s\n- **Nome**: %s\n- **Qualidade**: %s\n- **URL**: %s\n\nPróximo esperado: **#%d (S%02dE%02d)**.\nEnvie outro arquivo ou clique em **Concluir Lote**.", season, ep, absNum, season, ep, absNum, titleLine, fileName, quality, link, nextAbs, nextS, nextE), getWaitingFilesMarkup(state))
		} else {
			season := state.Season
			ep := state.CurrentEp

			caption := ""
			if u.EffectiveMessage != nil {
				caption = u.EffectiveMessage.Text
			}

			detSeason, detEp, isChapter := DetectSeasonAndEpisode(fileName, caption)
			if isChapter {
				// Chapter exception: Force season 1, ignore multiple seasons
				season = 1
				if detEp > 0 {
					ep = detEp
				}
			} else {
				if detSeason > 0 {
					season = detSeason
				}
				if detEp > 0 {
					ep = detEp
				}
			}

			if season < 1 {
				season = 1
			}
			if ep < 1 {
				ep = 1
			}

			seasonStr := strconv.Itoa(season)
			epStr := strconv.Itoa(ep)

			if state.SeriesStreams == nil {
				state.SeriesStreams = make(map[string]map[string][]StreamObj)
			}
			if state.SeriesStreams[seasonStr] == nil {
				state.SeriesStreams[seasonStr] = make(map[string][]StreamObj)
			}
			state.SeriesStreams[seasonStr][epStr] = append(state.SeriesStreams[seasonStr][epStr], streamObj)

			state.Season = season
			state.CurrentEp = ep + 1
			sendLoteResponse(ctx, u, fmt.Sprintf("📺 **S%02dE%02d** adicionado!\n- **Temporada**: %d\n- **Episódio**: %d\n- **Nome**: %s\n- **Qualidade**: %s\n- **URL**: %s\n\nPróximo esperado: S%02dE%02d.\nEnvie outro arquivo ou clique em **Concluir Lote**.", season, ep, season, ep, fileName, quality, link, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
		}
		return true, nil
	}

	return false, nil
}

func startLote(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return dispatcher.EndGroups
	}

	if len(config.ValueOf.AllowedUsers) != 0 && !utils.Contains(config.ValueOf.AllowedUsers, chatId) {
		sendLoteResponse(ctx, u, "Você não está autorizado a usar este bot.", nil)
		return dispatcher.EndGroups
	}

	colab := extractColaborador(u)

	loteMutex.Lock()
	loteStates[chatId] = &LoteState{
		Step:        2,
		Colaborador: colab,
	}
	loteMutex.Unlock()

	sendTypePrompt(ctx, u)
	return dispatcher.EndGroups
}

func concluirLote(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	return concluirLoteHelper(ctx, u, chatId)
}

func concluirLoteHelper(ctx *ext.Context, u *ext.Update, chatId int64) error {
	loteMutex.Lock()
	state, exists := loteStates[chatId]
	loteMutex.Unlock()

	if !exists {
		sendLoteResponse(ctx, u, "Você não tem nenhum lote ativo. Digite `/lote` para iniciar.", nil)
		return dispatcher.EndGroups
	}

	if state.Step < 8 {
		sendLoteResponse(ctx, u, "Você não concluiu as configurações do lote. Responda às perguntas anteriores ou envie `/cancelar`.", nil)
		return dispatcher.EndGroups
	}

	var finalJSONMap map[string]interface{}
	if state.Type == "movie" {
		finalJSONMap = map[string]interface{}{
			"id":          state.ImdbID,
			"type":        "movie",
			"streams":     state.MovieStreams,
			"colaborador": state.Colaborador,
		}
	} else {
		finalJSONMap = map[string]interface{}{
			"id":          state.ImdbID,
			"type":        "series",
			"streams":     state.SeriesStreams,
			"colaborador": state.Colaborador,
		}
	}

	jsonBytes, err := json.MarshalIndent(finalJSONMap, "", "    ")
	if err != nil {
		sendLoteResponse(ctx, u, fmt.Sprintf("Erro ao gerar JSON: %s", err.Error()), nil)
		return dispatcher.EndGroups
	}
	jsonStr := string(jsonBytes)

	replyMsg := fmt.Sprintf("📦 **Lote Concluído com sucesso!**\n\n**JSON gerado:**\n```json\n%s\n```", jsonStr)
	sendLoteResponse(ctx, u, replyMsg, nil)

	// Upload and send JSON file
	upd := uploader.NewUploader(ctx.Raw)
	fileName := fmt.Sprintf("%s.json", state.ImdbID)
	f, uploadErr := upd.FromBytes(ctx, fileName, jsonBytes)
	if uploadErr != nil {
		utils.Logger.Sugar().Warnf("Failed to upload JSON file to Telegram: %v", uploadErr)
	} else {
		peer := ctx.PeerStorage.GetInputPeerById(chatId)
		if peer.Zero() {
			peer = &tg.InputPeerUser{UserID: chatId}
		}
		mediaOption := message.UploadedDocument(f).MIME("application/json").Filename(fileName)
		_, sendMediaErr := ctx.Sender.To(peer).Media(ctx, mediaOption)
		if sendMediaErr != nil {
			utils.Logger.Sugar().Warnf("Failed to send JSON document to user: %v", sendMediaErr)
		}
	}

	if config.ValueOf.FenixFlixAPIURL != "" {
		sendLoteResponse(ctx, u, "📤 Enviando lote automaticamente para o banco de dados do FenixFlix...", nil)
		err := uploadToFenixFlix(config.ValueOf.FenixFlixAPIURL, state.ImdbID, jsonStr, config.ValueOf.FenixFlixPassword)
		if err != nil {
			sendLoteResponse(ctx, u, fmt.Sprintf("❌ Erro ao enviar para o FenixFlix: %s", err.Error()), nil)
		} else {
			sendLoteResponse(ctx, u, "✅ Lote enviado para o FenixFlix com sucesso!", nil)
		}
	}

	loteMutex.Lock()
	delete(loteStates, chatId)
	loteMutex.Unlock()

	return dispatcher.EndGroups
}

func cancelarLote(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	return cancelarLoteHelper(ctx, u, chatId)
}

func cancelarLoteHelper(ctx *ext.Context, u *ext.Update, chatId int64) error {
	loteMutex.Lock()
	_, exists := loteStates[chatId]
	if exists {
		delete(loteStates, chatId)
	}
	loteMutex.Unlock()

	if exists {
		sendLoteResponse(ctx, u, "📦 Lote cancelado.", nil)
	} else {
		sendLoteResponse(ctx, u, "Nenhum lote ativo encontrado para cancelar.", nil)
	}
	return dispatcher.EndGroups
}

func voltarLote(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return dispatcher.EndGroups
	}

	loteMutex.Lock()
	state, exists := loteStates[chatId]
	loteMutex.Unlock()

	if !exists {
		sendLoteResponse(ctx, u, "Você não tem nenhum lote ativo no momento.", nil)
		return dispatcher.EndGroups
	}

	state.Mutex.Lock()
	defer state.Mutex.Unlock()

	handleVoltar(ctx, u, state)
	return dispatcher.EndGroups
}

func audioLote(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return dispatcher.EndGroups
	}

	loteMutex.Lock()
	state, exists := loteStates[chatId]
	loteMutex.Unlock()

	if !exists {
		sendLoteResponse(ctx, u, "Você não tem nenhum lote ativo no momento.", nil)
		return dispatcher.EndGroups
	}

	state.Mutex.Lock()
	defer state.Mutex.Unlock()

	if state.Step < 5 {
		sendLoteResponse(ctx, u, "Você ainda não definiu o título da mídia.", nil)
		return dispatcher.EndGroups
	}

	state.EditingAudio = true
	state.Step = 5
	sendAudioPrompt(ctx, u, state.Title, state.ImdbID, true)
	return dispatcher.EndGroups
}

func handleLoteCallbackQuery(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	query := u.CallbackQuery
	if query == nil {
		return nil
	}

	loteMutex.Lock()
	state, exists := loteStates[chatId]
	loteMutex.Unlock()

	if !exists {
		ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			Alert:   true,
			QueryID: query.QueryID,
			Message: "Lote não ativo.",
		})
		return nil
	}

	state.Mutex.Lock()
	defer state.Mutex.Unlock()

	data := string(query.Data)
	ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
		QueryID: query.QueryID,
	})

	switch {
	case data == "lote_skip_nick":
		if state.Step == 1 {
			if user := u.EffectiveUser(); user != nil {
				state.Colaborador = user.FirstName
			} else {
				state.Colaborador = "Colaborador"
			}
			state.Step = 2
			sendTypePrompt(ctx, u)
		}
	case data == "lote_type_series":
		if state.Step == 2 {
			state.Type = "series"
			state.Step = 3
			sendSearchPrompt(ctx, u, "series")
		}
	case data == "lote_type_movie":
		if state.Step == 2 {
			state.Type = "movie"
			state.Step = 3
			sendSearchPrompt(ctx, u, "movie")
		}
	case data == "lote_back_to_type":
		state.Step = 2
		sendTypePrompt(ctx, u)
	case data == "lote_back_to_search":
		state.Step = 3
		sendSearchPrompt(ctx, u, state.Type)
	case data == "lote_back_from_audio":
		if len(state.SearchResults) > 0 {
			state.Step = 4
			sendMatchSelectionPrompt(ctx, u, state.SearchResults)
		} else {
			state.Step = 3
			sendSearchPrompt(ctx, u, state.Type)
		}
	case data == "lote_back_to_audio":
		state.Step = 5
		state.EditingAudio = false
		sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
	case data == "lote_back_to_season":
		state.Step = 6
		sendSeasonPrompt(ctx, u)
	case data == "lote_change_audio":
		state.EditingAudio = true
		state.Step = 5
		sendAudioPrompt(ctx, u, state.Title, state.ImdbID, true)
	case data == "lote_cancel_audio_change":
		state.EditingAudio = false
		state.Step = 8
		sendLoteResponse(ctx, u, fmt.Sprintf("Alteração cancelada. Áudio mantido: **%s**.\n\nEnvie os arquivos de vídeo ou clique em **Concluir Lote**.", state.Audio), getWaitingFilesMarkup(state))
	case data == "lote_back_from_files":
		if state.Step == 8 {
			if state.Type == "series" {
				if len(state.SeriesStreams) == 0 {
					state.Step = 7
					if state.IsAbsoluteEp {
						uniqueSeasons := make(map[int]bool)
						for _, v := range state.SeriesVideos {
							uniqueSeasons[v.Season] = true
						}
						sendAbsoluteEpPrompt(ctx, u, state, len(uniqueSeasons), len(state.SeriesVideos))
					} else {
						sendEpPrompt(ctx, u)
					}
				} else {
					sendLoteResponse(ctx, u, "Já existem arquivos adicionados. Para alterar o áudio, clique em **🔊 Alterar Áudio**.", getWaitingFilesMarkup(state))
				}
			} else {
				if len(state.MovieStreams) == 0 {
					state.Step = 5
					state.EditingAudio = false
					sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
				} else {
					sendLoteResponse(ctx, u, "Já existem arquivos adicionados. Para alterar o áudio, clique em **🔊 Alterar Áudio**.", getWaitingFilesMarkup(state))
				}
			}
		}
	case strings.HasPrefix(data, "lote_sel_"):
		if state.Step == 4 {
			id := strings.TrimPrefix(data, "lote_sel_")
			var selected CinemetaSearchResult
			found := false
			for _, r := range state.SearchResults {
				if r.ID == id {
					selected = r
					found = true
					break
				}
			}
			if found {
				state.ImdbID = selected.ID
				state.Title = selected.Name
				if state.Type == "series" && state.ImdbID != "" {
					go func(cid string) {
						vids, err := fetchCinemetaSeriesVideos(cid)
						if err == nil && len(vids) > 0 {
							state.Mutex.Lock()
							if state.ImdbID == cid && len(state.SeriesVideos) == 0 {
								state.SeriesVideos = vids
							}
							state.Mutex.Unlock()
						}
					}(state.ImdbID)
				}
				state.Step = 5
				sendAudioPrompt(ctx, u, state.Title, state.ImdbID, false)
			}
		}
	case strings.HasPrefix(data, "lote_audio_"):
		if state.Step == 5 {
			audio := strings.TrimPrefix(data, "lote_audio_")
			oldAudio := state.Audio
			state.Audio = audio

			if state.EditingAudio {
				state.EditingAudio = false
				state.Step = 8
				applyAudioUpdate(state, oldAudio, state.Audio)
				sendLoteResponse(ctx, u, fmt.Sprintf("🔊 Áudio alterado com sucesso para: **%s**!\n\nEnvie os arquivos de vídeo ou clique em **Concluir Lote**.", state.Audio), getWaitingFilesMarkup(state))
				return nil
			}

			if state.Type == "series" {
				state.Step = 6
				sendSeasonPrompt(ctx, u)
			} else {
				if state.MovieStreams == nil {
					state.MovieStreams = []StreamObj{}
				}
				state.Step = 8
				msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Filme\n- **Título**: %s (%s)\n- **Áudio**: %s\n\nAgora, **envie os arquivos de vídeo** para este lote.\n\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
				sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
			}
		}
	case data == "lote_absolute_ep":
		if state.Step == 6 {
			activateAbsoluteEpMode(ctx, u, state)
		}
	case data == "lote_skip_season":
		if state.Step == 6 {
			state.AutoDetectSeason = true
			state.Season = 1
			state.Step = 7
			sendEpPrompt(ctx, u)
		}
	case data == "lote_skip_abs_ep":
		if state.Step == 7 && state.IsAbsoluteEp {
			state.AutoDetectEp = true
			state.CurrentAbsoluteEp = 1
			sNext, eNext, _ := state.MapAbsoluteToSeasonEp(1)
			state.Season = sNext
			state.CurrentEp = eNext
			state.SeriesStreams = make(map[string]map[string][]StreamObj)
			state.Step = 8
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série (Episódios Absolutos)\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Episódio Inicial**: Automático (detectado do arquivo)\n\nAgora, **envie os arquivos de vídeo**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
		}
	case data == "lote_skip_ep":
		if state.Step == 7 {
			state.AutoDetectEp = true
			state.CurrentEp = 1
			state.SeriesStreams = make(map[string]map[string][]StreamObj)
			state.Step = 8
			seasonDesc := fmt.Sprintf("%d", state.Season)
			if state.AutoDetectSeason {
				seasonDesc = "Automática (pelo arquivo/legenda)"
			}
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Temporada**: %s\n- **Episódio Inicial**: Automático\n\nAgora, **envie os arquivos de vídeo**.\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio, seasonDesc)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
		}
	case data == "lote_concluir":
		concluirLoteHelper(ctx, u, chatId)
	case data == "lote_cancelar":
		cancelarLoteHelper(ctx, u, chatId)
	case data == "lote_inc_season":
		state.Season++
		state.CurrentEp = 1
		sendLoteResponse(ctx, u, fmt.Sprintf("Temporada alterada para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.Season, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
	case data == "lote_dec_season":
		if state.Season > 1 {
			state.Season--
			state.CurrentEp = 1
			sendLoteResponse(ctx, u, fmt.Sprintf("Temporada alterada para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.Season, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
		}
	case data == "lote_inc_ep":
		state.CurrentEp++
		sendLoteResponse(ctx, u, fmt.Sprintf("Próximo episódio esperado alterado para **%d**.\nEsperado: **S%dE%d**.", state.CurrentEp, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
	case data == "lote_dec_ep":
		if state.CurrentEp > 1 {
			state.CurrentEp--
			sendLoteResponse(ctx, u, fmt.Sprintf("Próximo episódio esperado alterado para **%d**.\nEsperado: **S%dE%d**.", state.CurrentEp, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
		}
	case data == "lote_inc_abs_ep":
		state.CurrentAbsoluteEp++
		sNext, eNext, _ := state.MapAbsoluteToSeasonEp(state.CurrentAbsoluteEp)
		state.Season = sNext
		state.CurrentEp = eNext
		sendLoteResponse(ctx, u, fmt.Sprintf("Próximo episódio absoluto alterado para **#%d**.\nEsperado: **S%02dE%02d** no IMDb.", state.CurrentAbsoluteEp, sNext, eNext), getWaitingFilesMarkup(state))
	case data == "lote_dec_abs_ep":
		if state.CurrentAbsoluteEp > 1 {
			state.CurrentAbsoluteEp--
			sNext, eNext, _ := state.MapAbsoluteToSeasonEp(state.CurrentAbsoluteEp)
			state.Season = sNext
			state.CurrentEp = eNext
			sendLoteResponse(ctx, u, fmt.Sprintf("Próximo episódio absoluto alterado para **#%d**.\nEsperado: **S%02dE%02d** no IMDb.", state.CurrentAbsoluteEp, sNext, eNext), getWaitingFilesMarkup(state))
		}
	case data == "lote_info_abs":
		sNext, eNext, t := state.MapAbsoluteToSeasonEp(state.CurrentAbsoluteEp)
		msg := fmt.Sprintf("Episódio #%d corresponde a S%02dE%02d no IMDb", state.CurrentAbsoluteEp, sNext, eNext)
		if t != "" {
			msg += ": " + t
		}
		ctx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
			Alert:   true,
			QueryID: query.QueryID,
			Message: msg,
		})
		return nil
	}

	return nil
}

func sendTypePrompt(ctx *ext.Context, u *ext.Update) {
	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "📺 Série / Anime",
						Data: []byte("lote_type_series"),
					},
					&tg.KeyboardButtonCallback{
						Text: "🎬 Filme",
						Data: []byte("lote_type_movie"),
					},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "❌ Cancelar",
						Data: []byte("lote_cancelar"),
					},
				},
			},
		},
	}
	sendLoteResponse(ctx, u, "Qual é o tipo de conteúdo?", markup)
}

func sendAudioPrompt(ctx *ext.Context, u *ext.Update, title, imdbID string, isEditing bool) {
	var backRow tg.KeyboardButtonRow
	if isEditing {
		backRow = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: "🔙 Voltar (Manter Atual)", Data: []byte("lote_cancel_audio_change")},
			},
		}
	} else {
		backRow = tg.KeyboardButtonRow{
			Buttons: []tg.KeyboardButtonClass{
				&tg.KeyboardButtonCallback{Text: "🔙 Voltar", Data: []byte("lote_back_from_audio")},
				&tg.KeyboardButtonCallback{Text: "❌ Cancelar", Data: []byte("lote_cancelar")},
			},
		}
	}

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Dublado", Data: []byte("lote_audio_Dublado")},
					&tg.KeyboardButtonCallback{Text: "Legendado", Data: []byte("lote_audio_Legendado")},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Dual Áudio", Data: []byte("lote_audio_Dual Áudio")},
				},
			},
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "Português (PT-BR)", Data: []byte("lote_audio_Português (PT-BR)")},
					&tg.KeyboardButtonCallback{Text: "English", Data: []byte("lote_audio_English")},
				},
			},
			backRow,
		},
	}

	promptText := fmt.Sprintf("Encontrado: **%s** (`%s`)\n\nEscolha o idioma/áudio nos botões abaixo:", title, imdbID)
	if isEditing {
		promptText = fmt.Sprintf("Alterando áudio para: **%s** (`%s`)\n\nEscolha o novo idioma/áudio nos botões abaixo:", title, imdbID)
	}
	sendLoteResponse(ctx, u, promptText, markup)
}

func getStreamLinkForMessage(ctx *ext.Context, u *ext.Update, chatId int64) (string, string, error) {
	update, err := utils.ForwardMessages(ctx, chatId, config.ValueOf.LogChannelID, u.EffectiveMessage.ID)
	if err != nil {
		return "", "", err
	}
	if len(update.Updates) < 2 {
		return "", "", fmt.Errorf("unexpected update structure from Telegram")
	}
	msgIDUpdate, ok := update.Updates[0].(*tg.UpdateMessageID)
	if !ok {
		return "", "", fmt.Errorf("unexpected update type")
	}
	messageID := msgIDUpdate.ID
	newMsg, ok := update.Updates[1].(*tg.UpdateNewChannelMessage)
	if !ok {
		return "", "", fmt.Errorf("unexpected channel message update")
	}
	msg, ok := newMsg.Message.(*tg.Message)
	if !ok {
		return "", "", fmt.Errorf("unexpected message type")
	}
	doc := msg.Media
	file, err := utils.FileFromMedia(doc)
	if err != nil {
		return "", "", err
	}
	fullHash := utils.PackFile(
		file.FileName,
		file.FileSize,
		file.MimeType,
		file.ID,
	)
	hash := utils.GetShortHash(fullHash)
	link := fmt.Sprintf("/stream/%d?hash=%s&d=true", messageID, hash)
	return link, file.FileName, nil
}

func detectQuality(media tg.MessageMediaClass, fileName string) string {
	if docMedia, ok := media.(*tg.MessageMediaDocument); ok && docMedia.Document != nil {
		if doc, ok := docMedia.Document.AsNotEmpty(); ok {
			for _, attr := range doc.Attributes {
				if video, ok := attr.(*tg.DocumentAttributeVideo); ok {
					if video.H >= 2160 || video.W >= 3840 {
						return "2160p"
					}
					if video.H >= 1080 || video.W >= 1920 {
						return "1080p"
					}
					if video.H >= 720 || video.W >= 1280 {
						return "720p"
					}
					if video.H >= 480 || video.W >= 854 {
						return "480p"
					}
					if video.H > 0 {
						return fmt.Sprintf("%dp", video.H)
					}
				}
			}
		}
	}

	fn := strings.ToLower(fileName)
	if strings.Contains(fn, "2160p") || strings.Contains(fn, "4k") {
		return "2160p"
	}
	if strings.Contains(fn, "1080p") {
		return "1080p"
	}
	if strings.Contains(fn, "720p") {
		return "720p"
	}
	if strings.Contains(fn, "480p") {
		return "480p"
	}
	if strings.Contains(fn, "360p") {
		return "360p"
	}

	return "1080p"
}

func queryCinemetaCatalog(contentType, query string) ([]CinemetaSearchResult, error) {
	escapedQuery := url.PathEscape(query)
	apiURL := fmt.Sprintf("https://v3-cinemeta.strem.io/catalog/%s/top/search=%s.json", contentType, escapedQuery)

	client := &http.Client{}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	var result struct {
		Metas []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ReleaseInfo string `json:"releaseInfo"`
		} `json:"metas"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var list []CinemetaSearchResult
	limit := len(result.Metas)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		m := result.Metas[i]
		list = append(list, CinemetaSearchResult{
			ID:   m.ID,
			Name: m.Name,
			Year: m.ReleaseInfo,
		})
	}
	return list, nil
}

func fetchCinemetaDetails(contentType, imdbID string) (string, error) {
	apiURL := fmt.Sprintf("https://v3-cinemeta.strem.io/meta/%s/%s.json", contentType, imdbID)

	client := &http.Client{}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}

	var result struct {
		Meta struct {
			Name string `json:"name"`
		} `json:"meta"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if result.Meta.Name == "" {
		return "", fmt.Errorf("not found")
	}

	return result.Meta.Name, nil
}

func activateAbsoluteEpMode(ctx *ext.Context, u *ext.Update, state *LoteState) error {
	if len(state.SeriesVideos) == 0 {
		videos, err := fetchCinemetaSeriesVideos(state.ImdbID)
		if err != nil || len(videos) == 0 {
			utils.Logger.Sugar().Warnf("Failed to fetch Cinemeta series videos for %s: %v", state.ImdbID, err)
		} else {
			state.SeriesVideos = videos
		}
	}

	uniqueSeasons := make(map[int]bool)
	for _, v := range state.SeriesVideos {
		uniqueSeasons[v.Season] = true
	}
	numSeasons := len(uniqueSeasons)
	totalEps := len(state.SeriesVideos)

	state.IsAbsoluteEp = true
	state.Step = 7
	return sendAbsoluteEpPrompt(ctx, u, state, numSeasons, totalEps)
}

func fetchCinemetaSeriesVideos(imdbID string) ([]CinemetaVideo, error) {
	apiURL := fmt.Sprintf("https://v3-cinemeta.strem.io/meta/series/%s.json", imdbID)

	client := &http.Client{}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	var result struct {
		Meta struct {
			Videos []struct {
				Season  int    `json:"season"`
				Episode int    `json:"episode"`
				Number  int    `json:"number"`
				Name    string `json:"name"`
			} `json:"videos"`
		} `json:"meta"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	type seKey struct {
		s int
		e int
	}
	seen := make(map[seKey]bool)
	var list []CinemetaVideo

	for _, v := range result.Meta.Videos {
		if v.Season <= 0 {
			continue
		}
		ep := v.Episode
		if ep == 0 {
			ep = v.Number
		}
		if ep <= 0 {
			continue
		}
		k := seKey{s: v.Season, e: ep}
		if seen[k] {
			continue
		}
		seen[k] = true
		list = append(list, CinemetaVideo{
			Season:  v.Season,
			Episode: ep,
			Title:   v.Name,
		})
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Season != list[j].Season {
			return list[i].Season < list[j].Season
		}
		return list[i].Episode < list[j].Episode
	})

	return list, nil
}

func uploadToFenixFlix(apiURL, imdbID, jsonContent, password string) error {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	fw, err := w.CreateFormField("nome")
	if err != nil {
		return err
	}
	if _, err = fw.Write([]byte(imdbID)); err != nil {
		return err
	}

	fw, err = w.CreateFormField("conteudo")
	if err != nil {
		return err
	}
	if _, err = fw.Write([]byte(jsonContent)); err != nil {
		return err
	}

	if password != "" {
		fw, err = w.CreateFormField("senha")
		if err != nil {
			return err
		}
		if _, err = fw.Write([]byte(password)); err != nil {
			return err
		}
	}

	w.Close()

	req, err := http.NewRequest("POST", apiURL, &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned status %s: %s", resp.Status, string(bodyBytes))
	}
	return nil
}
