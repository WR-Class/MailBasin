package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/store"
)

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "realtime_unsupported", "当前连接不支持实时事件流")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	channel, unsubscribe := s.store.SubscribeChanges(128)
	defer unsubscribe()

	lastSequence, replay := realtimeSequence(r)
	if !replay {
		var err error
		lastSequence, err = s.store.LatestChangeSequence()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "realtime_sequence_failed", "读取实时变更序号失败")
			return
		}
	}

	if _, err := fmt.Fprint(w, "retry: 2000\n\n"); err != nil {
		return
	}
	flusher.Flush()

	if replay {
		if err := s.replayRealtimeChanges(w, flusher, &lastSequence); err != nil {
			s.log.Warn("回放实时变更记录失败", "错误", err)
			return
		}
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := s.replayRealtimeChanges(w, flusher, &lastSequence); err != nil {
				s.log.Warn("补齐实时变更记录失败", "错误", err)
				return
			}
			if _, err := fmt.Fprintf(w, ": 心跳 %d\n\n", time.Now().Unix()); err != nil {
				return
			}
			flusher.Flush()
		case change, open := <-channel:
			if !open {
				return
			}
			if change.Sequence <= lastSequence {
				continue
			}
			if change.Sequence > lastSequence+1 {
				if err := s.replayRealtimeChanges(w, flusher, &lastSequence); err != nil {
					s.log.Warn("补齐实时变更记录失败", "错误", err)
					return
				}
				if change.Sequence <= lastSequence {
					continue
				}
			}
			if err := writeRealtimeChange(w, flusher, change); err != nil {
				return
			}
			lastSequence = change.Sequence
		}
	}
}

func realtimeSequence(r *http.Request) (int64, bool) {
	value := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if value == "" {
		if values, ok := r.URL.Query()["after"]; ok && len(values) > 0 {
			value = strings.TrimSpace(values[0])
		} else {
			return 0, false
		}
	}
	sequence, _ := strconv.ParseInt(value, 10, 64)
	if sequence < 0 {
		sequence = 0
	}
	return sequence, true
}

func (s *Server) replayRealtimeChanges(w http.ResponseWriter, flusher http.Flusher, lastSequence *int64) error {
	for {
		changes, err := s.store.ChangesAfter(*lastSequence, 500)
		if err != nil {
			return err
		}
		for _, change := range changes {
			if err := writeRealtimeChange(w, flusher, change); err != nil {
				return err
			}
			*lastSequence = change.Sequence
		}
		if len(changes) < 500 {
			return nil
		}
	}
}

func writeRealtimeChange(w http.ResponseWriter, flusher http.Flusher, change store.Change) error {
	data, err := json.Marshal(change)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", change.Sequence, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
