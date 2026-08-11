package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
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

type LoteState struct {
	Step          int // 1: Nick, 2: Type, 3: Search, 4: MatchSelection, 5: Audio, 6: Season, 7: StartEp, 8: Files
	Colaborador   string
	Type          string // "movie" or "series"
	ImdbID        string
	Title         string
	Audio         string
	Season        int
	CurrentEp     int
	MovieStreams  []StreamObj
	SeriesStreams map[string]map[string][]StreamObj // Season -> Ep -> []StreamObj
	SearchResults []CinemetaSearchResult
	Mutex         sync.Mutex // Per-user lock for processing files & messages
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

func getWaitingFilesMarkup(state *LoteState) tg.ReplyMarkupClass {
	if state.Type == "movie" {
		return &tg.ReplyInlineMarkup{
			Rows: []tg.KeyboardButtonRow{
				{
					Buttons: []tg.KeyboardButtonClass{
						&tg.KeyboardButtonCallback{Text: "📦 Concluir Lote", Data: []byte("lote_concluir")},
						&tg.KeyboardButtonCallback{Text: "❌ Cancelar", Data: []byte("lote_cancelar")},
					},
				},
			},
		}
	}

	// Series/Anime markup
	row1 := tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: fmt.Sprintf("➕ Temp (S%d)", state.Season+1), Data: []byte("lote_inc_season")},
		},
	}
	if state.Season > 1 {
		row1.Buttons = append(row1.Buttons, &tg.KeyboardButtonCallback{Text: fmt.Sprintf("➖ Temp (S%d)", state.Season-1), Data: []byte("lote_dec_season")})
	}

	row2 := tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: fmt.Sprintf("➕ Ep (E%d)", state.CurrentEp+1), Data: []byte("lote_inc_ep")},
		},
	}
	if state.CurrentEp > 1 {
		row2.Buttons = append(row2.Buttons, &tg.KeyboardButtonCallback{Text: fmt.Sprintf("➖ Ep (E%d)", state.CurrentEp-1), Data: []byte("lote_dec_ep")})
	}

	return &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			row1,
			row2,
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{Text: "📦 Concluir Lote", Data: []byte("lote_concluir")},
					&tg.KeyboardButtonCallback{Text: "❌ Cancelar", Data: []byte("lote_cancelar")},
				},
			},
		},
	}
}

func (m *command) LoadLote(dispatcher dispatcher.Dispatcher) {
	log := m.log.Named("lote")
	defer log.Sugar().Info("Loaded")
	dispatcher.AddHandler(handlers.NewCommand("lote", startLote))
	dispatcher.AddHandler(handlers.NewCommand("concluido", concluirLote))
	dispatcher.AddHandler(handlers.NewCommand("cancelar", cancelarLote))
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

	// If it's a command, let other handlers process it
	if strings.HasPrefix(text, "/") {
		if state.Step == 8 && (strings.HasPrefix(text, "/temporada ") || strings.HasPrefix(text, "/episodio ")) {
			if state.Type != "series" {
				sendLoteResponse(ctx, u, "Esta opção só é válida para Séries/Animes.", nil)
				return true, nil
			}
			if strings.HasPrefix(text, "/temporada ") {
				valStr := strings.TrimPrefix(text, "/temporada ")
				season, err := strconv.Atoi(valStr)
				if err != nil || season < 1 {
					sendLoteResponse(ctx, u, "Temporada inválida. Digite um número positivo: (Ex: `/temporada 2`)", nil)
					return true, nil
				}
				state.Season = season
				state.CurrentEp = 1
				sendLoteResponse(ctx, u, fmt.Sprintf("Temporada alterada para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.Season, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
				return true, nil
			}
			if strings.HasPrefix(text, "/episodio ") {
				valStr := strings.TrimPrefix(text, "/episodio ")
				ep, err := strconv.Atoi(valStr)
				if err != nil || ep < 1 {
					sendLoteResponse(ctx, u, "Episódio inválido. Digite um número positivo: (Ex: `/episodio 5`)", nil)
					return true, nil
				}
				state.CurrentEp = ep
				sendLoteResponse(ctx, u, fmt.Sprintf("Episódio atual alterado para **%d**.\nPróximo episódio esperado: **S%dE%d**.", state.CurrentEp, state.Season, state.CurrentEp), getWaitingFilesMarkup(state))
				return true, nil
			}
		}
		return false, nil
	}

	switch state.Step {
	case 1: // Nick
		if strings.ToLower(text) == "pular" {
			if user := u.EffectiveUser(); user != nil {
				state.Colaborador = user.FirstName
			} else {
				state.Colaborador = "Colaborador"
			}
		} else {
			state.Colaborador = text
		}
		state.Step = 2
		sendTypePrompt(ctx, u)
		return true, nil

	case 2: // Type
		if text == "1" {
			state.Type = "series"
			state.Step = 3
			sendLoteResponse(ctx, u, "Tipo configurado: Série / Anime.\n\nAgora digite o Nome ou o ID IMDb (começando com 'tt'):", nil)
		} else if text == "2" {
			state.Type = "movie"
			state.Step = 3
			sendLoteResponse(ctx, u, "Tipo configurado: Filme.\n\nAgora digite o Nome ou o ID IMDb (começando com 'tt'):", nil)
		} else {
			sendLoteResponse(ctx, u, "Opção inválida. Por favor, responda com:\n1. Série / Anime\n2. Filme", nil)
		}
		return true, nil

	case 3: // Search Query or IMDb ID
		if strings.HasPrefix(text, "tt") {
			title, err := fetchCinemetaDetails(state.Type, text)
			if err != nil {
				sendLoteResponse(ctx, u, "Não encontrei esse ID IMDb no Cinemeta. Digite o nome para buscar novamente:", nil)
				return true, nil
			}
			state.ImdbID = text
			state.Title = title
			state.Step = 5
			sendAudioPrompt(ctx, u, state.Title, state.ImdbID)
		} else {
			results, err := queryCinemetaCatalog(state.Type, text)
			if err != nil {
				sendLoteResponse(ctx, u, fmt.Sprintf("Erro ao buscar no Cinemeta: %s. Tente novamente:", err.Error()), nil)
				return true, nil
			}
			if len(results) == 0 {
				sendLoteResponse(ctx, u, "Nenhum resultado encontrado. Digite outro nome para buscar novamente:", nil)
				return true, nil
			}
			state.SearchResults = results
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
			state.Step = 4
			sendLoteResponse(ctx, u, "Escolha o item correto clicando em um dos botões abaixo:", &tg.ReplyInlineMarkup{Rows: rows})
		}
		return true, nil

	case 4: // Select match
		idx, err := strconv.Atoi(text)
		if err != nil || idx < 1 || idx > len(state.SearchResults) {
			sendLoteResponse(ctx, u, fmt.Sprintf("Opção inválida. Digite um número de 1 a %d:", len(state.SearchResults)), nil)
			return true, nil
		}
		selected := state.SearchResults[idx-1]
		state.ImdbID = selected.ID
		state.Title = selected.Name
		state.Step = 5
		sendAudioPrompt(ctx, u, state.Title, state.ImdbID)
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
		state.Audio = audio

		if state.Type == "series" {
			state.Step = 6
			sendLoteResponse(ctx, u, "Qual é a temporada do lote? (Ex: 1)", nil)
		} else {
			state.MovieStreams = []StreamObj{}
			state.Step = 8
			msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Filme\n- **Título**: %s (%s)\n- **Áudio**: %s\n\nAgora, **envie os arquivos de vídeo** para este lote.\n\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
			sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
		}
		return true, nil

	case 6: // Season
		season, err := strconv.Atoi(text)
		if err != nil || season < 1 {
			sendLoteResponse(ctx, u, "Temporada inválida. Digite um número positivo:", nil)
			return true, nil
		}
		state.Season = season
		state.Step = 7
		sendLoteResponse(ctx, u, "Qual é o número do episódio inicial? (Ex: 1)", nil)
		return true, nil

	case 7: // Start ep
		ep, err := strconv.Atoi(text)
		if err != nil || ep < 1 {
			sendLoteResponse(ctx, u, "Episódio inválido. Digite um número positivo:", nil)
			return true, nil
		}
		state.CurrentEp = ep
		state.SeriesStreams = make(map[string]map[string][]StreamObj)
		state.Step = 8
		msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Série\n- **Título**: %s (%s)\n- **Áudio**: %s\n- **Temporada**: %d\n- **Episódio Inicial**: %d\n\nAgora, **envie os arquivos de vídeo em ordem**.\nO número do episódio será incrementado a cada envio.\n\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio, state.Season, state.CurrentEp)
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
		} else {
			seasonStr := strconv.Itoa(state.Season)
			epStr := strconv.Itoa(state.CurrentEp)

			if state.SeriesStreams[seasonStr] == nil {
				state.SeriesStreams[seasonStr] = make(map[string][]StreamObj)
			}
			state.SeriesStreams[seasonStr][epStr] = append(state.SeriesStreams[seasonStr][epStr], streamObj)

			oldEp := state.CurrentEp
			state.CurrentEp++
			sendLoteResponse(ctx, u, fmt.Sprintf("📺 Episódio %d adicionado!\n- **Nome**: %s\n- **Qualidade**: %s\n- **URL**: %s\n\nPróximo esperado: %d.\nEnvie outro arquivo ou clique em **Concluir Lote**.", oldEp, fileName, quality, link, state.CurrentEp), getWaitingFilesMarkup(state))
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

	loteMutex.Lock()
	loteStates[chatId] = &LoteState{
		Step: 1,
	}
	loteMutex.Unlock()

	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{
			{
				Buttons: []tg.KeyboardButtonClass{
					&tg.KeyboardButtonCallback{
						Text: "Pular (Usar nome do Telegram)",
						Data: []byte("lote_skip_nick"),
					},
				},
			},
		},
	}
	sendLoteResponse(ctx, u, "📦 **Lote Iniciado!**\n\nPor favor, digite seu **nick de colaborador**:", markup)
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
			sendLoteResponse(ctx, u, "Tipo configurado: Série / Anime.\n\nAgora digite o Nome ou o ID IMDb (começando com 'tt'):", nil)
		}
	case data == "lote_type_movie":
		if state.Step == 2 {
			state.Type = "movie"
			state.Step = 3
			sendLoteResponse(ctx, u, "Tipo configurado: Filme.\n\nAgora digite o Nome ou o ID IMDb (começando com 'tt'):", nil)
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
				state.Step = 5
				sendAudioPrompt(ctx, u, state.Title, state.ImdbID)
			}
		}
	case strings.HasPrefix(data, "lote_audio_"):
		if state.Step == 5 {
			audio := strings.TrimPrefix(data, "lote_audio_")
			state.Audio = audio

			if state.Type == "series" {
				state.Step = 6
				sendLoteResponse(ctx, u, "Qual é a temporada do lote? (Ex: 1)", nil)
			} else {
				state.MovieStreams = []StreamObj{}
				state.Step = 8
				msgStr := fmt.Sprintf("✅ **Configurações Concluídas!**\n\n- **Colaborador**: %s\n- **Tipo**: Filme\n- **Título**: %s (%s)\n- **Áudio**: %s\n\nAgora, **envie os arquivos de vídeo** para este lote.\n\nQuando terminar, envie `/concluido`.", state.Colaborador, state.Title, state.ImdbID, state.Audio)
				sendLoteResponse(ctx, u, msgStr, getWaitingFilesMarkup(state))
			}
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
		},
	}
	sendLoteResponse(ctx, u, "Qual é o tipo de conteúdo?", markup)
}

func sendAudioPrompt(ctx *ext.Context, u *ext.Update, title, imdbID string) {
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
		},
	}
	sendLoteResponse(ctx, u, fmt.Sprintf("Encontrado: %s (%s)\n\nEscolha o idioma/áudio nos botões abaixo:", title, imdbID), markup)
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
	if docMedia, ok := media.(*tg.MessageMediaDocument); ok {
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
