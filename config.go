package main

const (
	guildID           = ""
	channelChatLobby  = ""
	channelLogAdmin   = ""
	channelPengumuman = ""
	channelAdmin      = ""
	channelNewMember  = ""

	adminRoleName     = ""
	magangRoleName    = ""
	stageShowRoleName = ""
	memberRoleName    = ""
	botMemberRoleName = ""
	logoFileName      = "logo.jpeg"
	databaseFileName  = "database.db"

	commandPing  = "!uhuy"
	commandLogin = "!login"
	commandRules = "g!aturan"
)

type professionOption struct {
	Label       string
	Description string
	Emoji       string
	Value       string
}

var roleTingkatan = []string{
	"",
	"",
	"",
	"",
	"",
}

var (
	kataSapaan = []string{"hello", ""}
	kataPamit  = []string{"bye", ""}
	kataBuruk  = []string{"f*ck", ""}

	karmaRoleNames = []string{
		"",
		"",
		"",
		"",
		"",
	}
)
