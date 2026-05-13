package ironic

import (
	"context"
	"time"

	"autopxe/internal/configdrive"
	"autopxe/internal/ironic/ipa"
	"autopxe/internal/node"
)

// driveDeploy is the single per-node deploy driver. It runs as a goroutine
// launched by handleHeartbeat the first time a heartbeat arrives for a node
// in StateNew. Subsequent heartbeats only refresh LastHeartbeat — they never
// dispatch additional commands.
func (s *Server) driveDeploy(n *node.Node) {
	ctx := context.Background() // detached from request; lifetime is the deploy

	// Wait briefly for callback URL to be present (set by RecordHeartbeat).
	snap := n.Get()
	if snap.CallbackURL == "" {
		s.logger.Warn("driver started without callback url", "uuid", n.UUID)
		return
	}

	if !n.Transition(node.StateNew, node.StateDeploying) {
		s.logger.Warn("driver could not transition new->deploying", "uuid", n.UUID, "state", n.State())
		return
	}

	client := ipa.New(snap.CallbackURL, n.AgentToken)

	// 1. prepare_image (image_info already carries os_hash_algo + os_hash_value)
	imageInfo := s.instanceInfo(n.UUID)

	driveStr, err := configdrive.Generate(n.UUID, n.Hostname,
		s.configDriveCfg.UserData,
		s.configDriveCfg.MetaData,
		s.configDriveCfg.NetworkData,
	)
	if err != nil {
		s.logger.Error("configdrive generation failed", "uuid", n.UUID, "err", err.Error())
		// config drive failure is not fatal — deploy without it
	}

	params := map[string]any{"image_info": imageInfo}
	if driveStr != "" {
		params["configdrive"] = driveStr
	}

	resp, err := client.SendCommand(ctx, "standby.prepare_image", params)
	if err != nil {
		s.logger.Error("prepare_image dispatch failed", "uuid", n.UUID, "err", err.Error())
		n.ForceState(node.StateFailed)
		return
	}
	cmdID, _ := resp["id"].(string)
	if cmdID == "" {
		s.logger.Error("prepare_image returned no id", "uuid", n.UUID, "resp", resp)
		n.ForceState(node.StateFailed)
		return
	}
	n.SetCommandID(cmdID)
	s.logger.Info("prepare_image dispatched", "uuid", n.UUID, "cmd", cmdID)

	// 2. poll
	if !s.pollUntilTerminal(ctx, n, client, cmdID) {
		return
	}

	// 3. run_image
	if !n.Transition(node.StateDeploying, node.StateRunImage) {
		s.logger.Warn("could not transition deploying->run_image", "uuid", n.UUID)
		return
	}
	if _, err := client.SendCommand(ctx, "standby.run_image", map[string]any{}); err != nil {
		s.logger.Error("run_image dispatch failed", "uuid", n.UUID, "err", err.Error())
		n.ForceState(node.StateFailed)
		return
	}
	n.ForceState(node.StateDone)

	// Persist the deployment so a subsequent PXE attempt by any of this
	// node's MACs is suppressed at the DHCP layer. Save errors are non-fatal
	// (we just lose the in-memory record on next restart) but logged.
	if s.tracker != nil {
		if err := s.tracker.MarkDeployed(n.MACs, n.UUID, s.imageHash); err != nil {
			s.logger.Error("persist deploy state", "uuid", n.UUID, "err", err.Error())
		}
	}

	s.logger.Info("deploy complete", "uuid", n.UUID, "macs", n.MACs)
}

func (s *Server) pollUntilTerminal(ctx context.Context, n *node.Node, client *ipa.Client, cmdID string) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(2 * time.Hour)

	for {
		status, err := client.GetCommandStatus(ctx, cmdID)
		if err != nil {
			s.logger.Warn("poll cmd failed", "uuid", n.UUID, "err", err.Error())
		} else {
			cmdStatus, _ := status["command_status"].(string)
			s.logger.Info("poll cmd", "uuid", n.UUID, "cmd", cmdID, "status", cmdStatus)
			switch cmdStatus {
			case "SUCCEEDED":
				return true
			case "FAILED":
				s.logger.Error("command failed", "uuid", n.UUID, "cmd", cmdID, "err", status["command_error"])
				n.ForceState(node.StateFailed)
				return false
			}
		}

		if time.Now().After(deadline) {
			s.logger.Error("deploy timeout", "uuid", n.UUID)
			n.ForceState(node.StateFailed)
			return false
		}

		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
