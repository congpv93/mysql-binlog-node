package main

import (
	"encoding/base64"
	"fmt"

	"log"
	"math/bits"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/go-mysql-org/go-mysql/schema"
	logger "github.com/siddontang/go-log/log"
)

type MysqlBinlogPosition struct {
	Name     string `json:"name"`
	Position uint32 `json:"position"`
}

type MysqlBinlogRowUpdate struct {
	Old map[string]any `json:"old"`
	New map[string]any `json:"new"`
}

type MysqlBinlogTable struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

type MysqlBinlogChangeEvent struct {
	BinlogPosition MysqlBinlogPosition    `json:"binlogPosition"`
	Table          MysqlBinlogTable       `json:"table"`
	Insert         []map[string]any       `json:"insert,omitempty"`
	Update         []MysqlBinlogRowUpdate `json:"update,omitempty"`
	Delete         []map[string]any       `json:"delete,omitempty"`
}

type canalEventHandler struct {
	canal  *canal.Canal
	events chan<- MysqlBinlogChangeEvent
	errors chan<- error
}

func (eh *canalEventHandler) OnRotate(_ *replication.EventHeader, _ *replication.RotateEvent) error {
	return nil
}

func (eh *canalEventHandler) OnTableChanged(_ *replication.EventHeader, _ string, _ string) error {
	return nil
}

func (eh *canalEventHandler) OnDDL(_ *replication.EventHeader, _ mysql.Position, _ *replication.QueryEvent) error {
	return nil
}

func (eh *canalEventHandler) OnRowsQueryEvent(_ *replication.RowsQueryEvent) error {
	return nil
}

type ErrorWithBinlogPosition struct {
	message        string
	BinlogPosition MysqlBinlogPosition
}

func (e ErrorWithBinlogPosition) Error() string {
	return e.message
}

func NewErrorWithBinlogPosition(message string, binlogPosition MysqlBinlogPosition) ErrorWithBinlogPosition {
	return ErrorWithBinlogPosition{
		message:        message,
		BinlogPosition: binlogPosition,
	}
}

func (eh *canalEventHandler) OnRow(event *canal.RowsEvent) error {
	binlogPosition := eh.canal.SyncedPosition()
	mysqlBinLogPosition := MysqlBinlogPosition{
		Name:     binlogPosition.Name,
		Position: binlogPosition.Pos,
	}

	parseRow := func(row []any) (map[string]any, error) {
		parsedRow := make(map[string]any)
		for idx, column := range event.Table.Columns {
			switch column.Type {
			case schema.TYPE_ENUM:
				if column.EnumValues == nil {
					return nil, NewErrorWithBinlogPosition("Received binlog event for enum, but could not find the corresponding string values", mysqlBinLogPosition)
				}

				if row[idx] == nil {
					parsedRow[column.Name] = nil
					continue
				}

				enumValue, ok := row[idx].(int64)
				if !ok {
					return nil, NewErrorWithBinlogPosition("Received binlog event for enum, but could not parse the value as int64", mysqlBinLogPosition)
				}

				if int(enumValue) > len(column.EnumValues) {
					return nil, NewErrorWithBinlogPosition("Received binlog event for enum, but the int value is out of range", mysqlBinLogPosition)
				}

				parsedRow[column.Name] = column.EnumValues[enumValue-1]
				continue
			case schema.TYPE_SET:
				if column.SetValues == nil {
					return nil, NewErrorWithBinlogPosition("Received binlog event for set, but could not find the corresponding string values", mysqlBinLogPosition)
				}

				if row[idx] == nil {
					parsedRow[column.Name] = nil
					continue
				}

				setValue, ok := row[idx].(int64)
				if !ok {
					return nil, NewErrorWithBinlogPosition("Received binlog event for set, but could not parse the value as int64", mysqlBinLogPosition)
				}

				if setValue >= (1 << uint(len(column.SetValues))) {
					return nil, NewErrorWithBinlogPosition("Received binlog event for set, but the int value is out of range", mysqlBinLogPosition)
				}

				setValues := make([]string, 0, bits.OnesCount(uint(setValue)))
				for i := 0; i < len(column.SetValues); i++ {
					if setValue&(1<<uint(i)) != 0 {
						setValues = append(setValues, column.SetValues[i])
					}
				}

				parsedRow[column.Name] = setValues
				continue
			}

			// Handle TEXT/BLOB data by checking if the value is []byte
			if row[idx] != nil {
				if textValue, ok := row[idx].([]byte); ok {
					// Try to decode base64 first, if it's base64 encoded
					if decoded, err := base64.StdEncoding.DecodeString(string(textValue)); err == nil {
						parsedRow[column.Name] = string(decoded)
					} else {
						// If not base64, just convert to string directly
						parsedRow[column.Name] = string(textValue)
					}
					continue
				}
			}

			parsedRow[column.Name] = row[idx]
		}
		return parsedRow, nil
	}

	changeEvent := MysqlBinlogChangeEvent{
		BinlogPosition: mysqlBinLogPosition,
		Table: MysqlBinlogTable{
			Schema: event.Table.Schema,
			Name:   event.Table.Name,
		},
	}

	switch event.Action {
	case canal.InsertAction:
		inserts := make([]map[string]any, 0, len(event.Rows))

		for _, row := range event.Rows {
			parsedRow, err := parseRow(row)
			if err != nil {
				eh.errors <- err
				return nil
			}
			inserts = append(inserts, parsedRow)
		}

		changeEvent.Insert = inserts
	case canal.UpdateAction:
		updates := make([]MysqlBinlogRowUpdate, 0, len(event.Rows)/2)

		for i := 0; i < len(event.Rows); i += 2 {
			oldRow, err := parseRow(event.Rows[i])
			if err != nil {
				eh.errors <- err
				return nil
			}

			if i+1 >= len(event.Rows) {
				eh.errors <- NewErrorWithBinlogPosition("Incomplete update pair in binlog event", mysqlBinLogPosition)
				return nil
			}

			newRow, err := parseRow(event.Rows[i+1])
			if err != nil {
				eh.errors <- err
				return nil
			}

			updates = append(updates, MysqlBinlogRowUpdate{
				Old: oldRow,
				New: newRow,
			})
		}

		changeEvent.Update = updates
	case canal.DeleteAction:
		deletes := make([]map[string]any, 0, len(event.Rows))

		for _, row := range event.Rows {
			parsedRow, err := parseRow(row)
			if err != nil {
				eh.errors <- err
				return nil
			}
			deletes = append(deletes, parsedRow)
		}

		changeEvent.Delete = deletes
	}
	eh.events <- changeEvent

	return nil
}

func (eh *canalEventHandler) OnXID(_ *replication.EventHeader, _ mysql.Position) error {
	return nil
}

func (eh *canalEventHandler) OnGTID(_ *replication.EventHeader, _ mysql.BinlogGTIDEvent) error {
	return nil
}

func (eh *canalEventHandler) OnPosSynced(_ *replication.EventHeader, _ mysql.Position, _ mysql.GTIDSet, _ bool) error {
	return nil
}

func (eh *canalEventHandler) String() string {
	return "canalEventHandler"
}

type channelLogHandler struct {
	c chan<- string
}

func (h *channelLogHandler) Write(b []byte) (n int, err error) {
	h.c <- string(b)
	return len(b), nil
}

func (h *channelLogHandler) Close() error {
	return nil
}

type MysqlBinlogSyncer struct {
	canal         *canal.Canal
	binlogEvents  chan MysqlBinlogChangeEvent
	binlogErrors  chan error
	loggingEvents chan string
}

func NewSyncer(config MysqlBinlogConfig) (*MysqlBinlogSyncer, error) {
	loggingEvents := make(chan string, 100)

	canalCfg := canal.NewDefaultConfig()
	canalCfg.Addr = fmt.Sprintf("%s:%d", config.Hostname, config.Port)
	canalCfg.User = config.Username
	canalCfg.Password = config.Password
	canalCfg.Charset = "utf8mb4"
	canalCfg.IncludeTableRegex = config.TableRegexes
	canalCfg.ParseTime = true
	canalCfg.Dump = canal.DumpConfig{}
	canalCfg.Logger = logger.New(&channelLogHandler{c: loggingEvents}, logger.Llevel|logger.Lfile)

	c, err := canal.NewCanal(canalCfg)
	if err != nil {
		return nil, err
	}
	binlogEvents := make(chan MysqlBinlogChangeEvent)
	binlogErrors := make(chan error)

	eventHandler := &canalEventHandler{
		canal:  c,
		errors: binlogErrors,
		events: binlogEvents,
	}
	c.SetEventHandler(eventHandler)

	var mysqlPosition mysql.Position
	if config.BinlogPosition == nil {
		masterPos, err := c.GetMasterPos()
		if err != nil {
			return nil, err
		}

		mysqlPosition = masterPos
	} else {
		mysqlPosition = *config.BinlogPosition
	}

	go func() {
		err := c.RunFrom(mysqlPosition)
		if err != nil {
			log.Printf("got error: %v", err)
		}
	}()

	return &MysqlBinlogSyncer{
		canal:         c,
		binlogEvents:  binlogEvents,
		binlogErrors:  binlogErrors,
		loggingEvents: loggingEvents,
	}, nil
}

func (s *MysqlBinlogSyncer) ChangeEvents() <-chan MysqlBinlogChangeEvent {
	return s.binlogEvents
}

func (s *MysqlBinlogSyncer) Errors() <-chan error {
	return s.binlogErrors
}

func (s *MysqlBinlogSyncer) LogEvents() <-chan string {
	return s.loggingEvents
}

func (s *MysqlBinlogSyncer) Close() {
	s.canal.Close()
	close(s.binlogEvents)
	close(s.loggingEvents)
}
