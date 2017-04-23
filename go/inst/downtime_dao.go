/*
   Copyright 2015 Shlomi Noach, courtesy Booking.com

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"fmt"
	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/db"
	"github.com/openark/golib/log"
)

// BeginDowntime will make mark an instance as downtimed (or override existing downtime period)
func BeginDowntime(instanceKey *InstanceKey, owner string, reason string, durationSeconds uint) error {
	if durationSeconds == 0 {
		durationSeconds = config.Config.MaintenanceExpireMinutes * 60
	}
	_, err := db.ExecOrchestrator(`
			insert
				into database_instance_downtime (
					hostname, port, downtime_active, begin_timestamp, end_timestamp, owner, reason
				) VALUES (
					?, ?, 1, NOW(), NOW() + INTERVAL ? SECOND, ?, ?
				)
				on duplicate key update
					downtime_active=values(downtime_active),
					begin_timestamp=values(begin_timestamp),
					end_timestamp=values(end_timestamp),
					owner=values(owner),
					reason=values(reason)
			`,
		instanceKey.Hostname,
		instanceKey.Port,
		durationSeconds,
		owner,
		reason,
	)
	if err != nil {
		return log.Errore(err)
	}

	AuditOperation("begin-downtime", instanceKey, fmt.Sprintf("owner: %s, reason: %s", owner, reason))

	return nil
}

// EndDowntime will remove downtime flag from an instance
func EndDowntime(instanceKey *InstanceKey) (wasDowntimed bool, err error) {
	res, err := db.ExecOrchestrator(`
			update
				database_instance_downtime
			set
				downtime_active = NULL,
				end_timestamp = NOW()
			where
				hostname = ?
				and port = ?
				and downtime_active = 1
			`,
		instanceKey.Hostname,
		instanceKey.Port,
	)
	if err != nil {
		return wasDowntimed, log.Errore(err)
	}

	if affected, _ := res.RowsAffected(); affected > 0 {
		wasDowntimed = true
		AuditOperation("end-downtime", instanceKey, "")
	}
	return wasDowntimed, err
}

// renewLostInRecoveryDowntime renews hosts who are downtimed due to being lost in recovery, such that
// their downtime never expires.
func renewLostInRecoveryDowntime() error {
	_, err := db.ExecOrchestrator(`
			update
				database_instance_downtime
			set
				end_timestamp = NOW() + INTERVAL ? SECOND
			where
				end_timestamp > NOW()
				and reason = ?
			`,
		config.LostInRecoveryDowntimeSeconds,
		DowntimeLostInRecoveryMessage,
	)

	return err
}

// expireLostInRecoveryDowntime expires downtime for servers who have been lost in recovery in the last,
// but are now replicating.
func expireLostInRecoveryDowntime() error {
	instances, err := ReadLostInRecoveryInstances("")
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.IsLastCheckValid && instance.ReplicaRunning() {
			_, err := db.ExecOrchestrator(`
				delete from
					database_instance_downtime
				where
					hostname = ?
					and port = ?
					`,
				instance.Key.Hostname, instance.Key.Port,
			)
			if err != nil {
				return err
			}
		}
	}

	return err
}

// ExpireDowntime will remove the maintenance flag on old downtimes
func ExpireDowntime() error {
	if err := renewLostInRecoveryDowntime(); err != nil {
		return log.Errore(err)
	}
	if err := expireLostInRecoveryDowntime(); err != nil {
		return log.Errore(err)
	}

	{
		res, err := db.ExecOrchestrator(`
			delete from
				database_instance_downtime
			where
				downtime_active is null
				and end_timestamp < NOW() - INTERVAL ? DAY
			`,
			config.Config.MaintenancePurgeDays,
		)
		if err != nil {
			return log.Errore(err)
		}
		if rowsAffected, _ := res.RowsAffected(); rowsAffected > 0 {
			AuditOperation("expire-downtime", nil, fmt.Sprintf("Purged %d historical entries", rowsAffected))
		}
	}
	{
		res, err := db.ExecOrchestrator(`
			update
				database_instance_downtime
			set
				downtime_active = NULL
			where
				downtime_active = 1
				and end_timestamp < NOW()
			`,
		)
		if err != nil {
			return log.Errore(err)
		}
		if rowsAffected, _ := res.RowsAffected(); rowsAffected > 0 {
			AuditOperation("expire-downtime", nil, fmt.Sprintf("Expired %d entries", rowsAffected))
		}
	}

	return nil
}
