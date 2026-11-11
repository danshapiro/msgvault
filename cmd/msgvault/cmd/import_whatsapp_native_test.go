package cmd

import (
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWhatsAppNativeIdentityReceiptRetainsLimitationsWithoutPartial(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "msgstore.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `
 CREATE TABLE jid(_id INTEGER PRIMARY KEY,user TEXT,server TEXT,raw_string TEXT);
 CREATE TABLE chat(_id INTEGER PRIMARY KEY,jid_row_id INTEGER,hidden INTEGER,subject TEXT,sort_timestamp INTEGER);
 CREATE TABLE message(_id INTEGER PRIMARY KEY,chat_row_id INTEGER,from_me INTEGER,key_id TEXT,sender_jid_row_id INTEGER,timestamp INTEGER,message_type INTEGER,text_data TEXT,status INTEGER,starred INTEGER,sort_id INTEGER);
 CREATE TABLE message_media(message_row_id INTEGER,mime_type TEXT,file_size INTEGER,file_path TEXT,width INTEGER,height INTEGER,media_duration INTEGER);
 CREATE TABLE jid_map(lid_row_id INTEGER,jid_row_id INTEGER);
 CREATE TABLE group_participant_user(_id INTEGER PRIMARY KEY,group_jid_row_id INTEGER,user_jid_row_id INTEGER,rank INTEGER,pending INTEGER);
 CREATE TABLE lid_display_name(lid_row_id INTEGER PRIMARY KEY,display_name TEXT,username TEXT);
 CREATE TABLE message_add_on(_id INTEGER PRIMARY KEY,parent_message_row_id INTEGER,sender_jid_row_id INTEGER);
 CREATE TABLE message_add_on_reaction(message_add_on_row_id INTEGER,reaction TEXT,sender_timestamp INTEGER);
 INSERT INTO jid VALUES(1,'12345','g.us','12345@g.us'),(2,'23456','lid','23456@lid');
 INSERT INTO chat VALUES(1,1,0,'Example group',1700000000000);
 INSERT INTO message(_id,chat_row_id,from_me,key_id,sort_id) VALUES(1,-1,0,'-1',0);
 INSERT INTO message VALUES(2,1,0,'example-native-message',2,1700000000000,0,'Example text',0,0,2);
 INSERT INTO group_participant_user VALUES(1,1,2,0,0);
 INSERT INTO lid_display_name VALUES(2,'','example_member');
 INSERT INTO message_add_on VALUES(1,2,2);
 INSERT INTO message_add_on_reaction VALUES(1,'👍',1700000001000);
 `)
	require.NoError(err)
	require.NoError(db.Close())
	home := t.TempDir()
	for attempt := range 2 {
		output, err := runWhatsAppIdentityCommand(t, home, path, true)
		require.NoError(err)
		result := decodeWhatsAppImportReceipt(t, output)
		assert.Equal("complete", result.Status)
		assert.Zero(result.Errors)
		if attempt == 1 {
			assert.Zero(result.Added)
		}
		_, encoded, found := strings.Cut(output, "WHATSAPP_IMPORT_RESULT=")
		require.True(found)
		encoded, _, _ = strings.Cut(encoded, "\n")
		var native struct {
			Counts     map[string]int64 `json:"identity_limitation_counts"`
			Identities int64            `json:"identities_without_phone"`
		}
		require.NoError(json.Unmarshal([]byte(encoded), &native))
		assert.Equal(map[string]int64{"group_roster_phone_unavailable": 1, "sender_phone_unavailable": 1, "reaction_phone_unavailable": 1}, native.Counts)
		assert.EqualValues(1, native.Identities)
		assert.Contains(output, "Phone mapping:")
	}
}
