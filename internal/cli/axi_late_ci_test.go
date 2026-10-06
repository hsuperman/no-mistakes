package cli

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"testing"
)

func TestAxiLateCIFindingUsesHeadBoundMethodAndRefusesOldDaemon(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "old-daemon", false: "head-bound-daemon"}[old], func(t *testing.T) {
			socket := filepath.Join(makeSocketSafeTempDir(t), "late.sock")
			server := ipc.NewServer()
			server.Handle(ipc.MethodRespond, func(context.Context, json.RawMessage) (interface{}, error) {
				t.Error("late finding sent as ordinary unbound approval")
				return &ipc.RespondResult{OK: true}, nil
			})
			if !old {
				server.Handle(ipc.MethodRespondLateCI, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.RespondParams
					if err := json.Unmarshal(raw, &params); err != nil {
						t.Error(err)
					}
					if params.ExpectedHeadSHA != "exact-head" || params.RunID != "run" || len(params.AddedFindings) != 1 {
						t.Errorf("head-bound request lost fields: %+v", params)
					}
					return &ipc.RespondResult{OK: true}, nil
				})
			}
			startIPCServer(t, server, socket)
			client := dialReady(t, socket)
			defer client.Close()
			err := sendLateCIRespond(client, &ipc.RespondParams{RunID: "run", ExpectedHeadSHA: "exact-head", Step: types.StepCI, Action: types.ActionFix, AddedFindings: []types.Finding{{Description: "new requirement"}}})
			if old {
				rpcErr, ok := err.(*ipc.RPCError)
				if !ok || rpcErr.Code != ipc.ErrMethodNotFound {
					t.Fatalf("old daemon accepted amendment: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
