package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyPlatformComplianceProductTriggerProfile(t *testing.T) {
	const legacy = "rereply_platform_compliance_write_guard"
	const futureMessages = legacy +
		",trg_messages_cleanup_read_cursors,trg_messages_ingestion_order"
	const futureReads = legacy + ",trg_conversation_reads_ingestion_order"

	for _, test := range []struct {
		name            string
		relationCount   int64
		messageTriggers string
		readTriggers    string
		want            platformComplianceProductTriggerProfile
		wantError       bool
	}{
		{
			name: "exact legacy", relationCount: 2,
			messageTriggers: legacy, readTriggers: legacy,
			want: platformComplianceLegacyTriggerProfile,
		},
		{
			name: "exact future", relationCount: 2,
			messageTriggers: futureMessages, readTriggers: futureReads,
			want: platformComplianceMessageCursorTriggerProfile,
		},
		{
			name: "mixed", relationCount: 2,
			messageTriggers: futureMessages, readTriggers: legacy,
			wantError: true,
		},
		{
			name: "partial", relationCount: 2,
			messageTriggers: legacy + ",trg_messages_ingestion_order", readTriggers: legacy,
			wantError: true,
		},
		{
			name: "extra", relationCount: 2,
			messageTriggers: futureMessages + ",zz_unreviewed", readTriggers: futureReads,
			wantError: true,
		},
		{
			name: "missing relation", relationCount: 1,
			messageTriggers: legacy, readTriggers: legacy,
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyPlatformComplianceProductTriggerProfile(
				test.relationCount,
				test.messageTriggers,
				test.readTriggers,
			)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFutureMessageCursorTriggerContractMetadataIsExact(t *testing.T) {
	messages := platformComplianceFutureMessageCursorTriggers["messages"]
	require.Len(t, messages, 2)
	assert.Equal(t, "trg_messages_ingestion_order", messages[0].name)
	assert.Equal(t, "rereply_set_message_ingestion_order", messages[0].function)
	assert.Equal(t, 23, messages[0].typeBits)
	assert.Equal(t, []string{"inbox_conversation_id"}, messages[0].updateColumns)
	assert.Equal(
		t,
		normalizePlatformComplianceFunctionBody(functionBodyFromCreateSQL(messageIngestionOrderFunctionSQL)),
		normalizePlatformComplianceFunctionBody(messages[0].functionBody),
	)
	assert.Equal(t, "trg_messages_cleanup_read_cursors", messages[1].name)
	assert.Equal(t, "rereply_cleanup_deleted_message_read_cursors", messages[1].function)
	assert.Equal(t, 11, messages[1].typeBits)
	assert.Empty(t, messages[1].updateColumns)

	reads := platformComplianceFutureMessageCursorTriggers["conversation_reads"]
	require.Len(t, reads, 1)
	assert.Equal(t, "trg_conversation_reads_ingestion_order", reads[0].name)
	assert.Equal(t, "rereply_set_conversation_read_ingestion_order", reads[0].function)
	assert.Equal(t, 23, reads[0].typeBits)
	assert.ElementsMatch(t, []string{"last_read_message_id", "last_read_at"}, reads[0].updateColumns)
}
