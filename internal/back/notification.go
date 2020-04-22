package back

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jmoiron/sqlx"
)

type NotificationRecipientType int

const (
	NotificationRecipientTypeDiscordChannel NotificationRecipientType = 0
	NotificationRecipientTypeDiscordUser    NotificationRecipientType = 1
)

type NotificationType int

const (
	NotificationTypeMatchSessionStatusUpdate NotificationType = iota
	NotificationTypeMatchSessionCountdown
	NotificationTypeMatchSessionEmpty
	NotificationTypeMatchSessionOddKick
	NotificationTypeMatchSeed
	NotificationTypeMatchEnd
)

type NotificationFile struct {
	Name        string
	ContentType string
	Reader      io.Reader
}

type Notification struct {
	RecipientType NotificationRecipientType
	Recipient     string
	Type          NotificationType
	Files         []NotificationFile

	body bytes.Buffer
}

func (n *Notification) Printf(str string, args ...interface{}) (int, error) {
	return fmt.Fprintf(&n.body, str, args...)
}

func (n *Notification) Print(args ...interface{}) (int, error) {
	return fmt.Fprint(&n.body, args...)
}

func (n *Notification) Read(p []byte) (int, error) {
	return n.body.Read(p)
}

func (n *Notification) Write(p []byte) (int, error) {
	return n.body.Write(p)
}

func NotificationTypeName(typ NotificationType) string {
	switch typ {
	case NotificationTypeMatchSessionStatusUpdate:
		return "MatchSessionStatusUpdate"
	case NotificationTypeMatchSessionEmpty:
		return "MatchSessionEmpty"
	case NotificationTypeMatchSessionOddKick:
		return "MatchSessionOddKick"
	case NotificationTypeMatchSeed:
		return "MatchSeed"
	case NotificationTypeMatchEnd:
		return "MatchEnd"
	default:
		return "invalid"
	}
}

func NotificationRecipientTypeName(typ NotificationRecipientType) string {
	switch typ {
	case NotificationRecipientTypeDiscordChannel:
		return "DiscordChannel"
	case NotificationRecipientTypeDiscordUser:
		return "DiscordUser"
	default:
		return "invalid"
	}
}

// For debugging purposes only.
func (n *Notification) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(
		&buf,
		"type %s, recipient type %s \"%s\"",
		NotificationTypeName(n.Type),
		NotificationRecipientTypeName(n.RecipientType),
		n.Recipient,
	)

	if len := len(n.Files); len > 0 {
		fmt.Fprintf(&buf, ", %d file(s)", len)
	}

	// HACK: Ensure its on one line (and safe to print)
	content, _ := json.Marshal(n.body.String())
	fmt.Fprintf(&buf, ", contents: %s", string(content))

	return buf.String()
}

func (b *Back) sendOddKickNotification(player Player) {
	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordUser,
		Recipient:     player.DiscordID.String,
		Type:          NotificationTypeMatchSessionOddKick,
	}

	notif.Printf(
		"Sorry %s, but there was an odd number of players and you were the last person to join.\n"+
			"You have been kicked out of the race, don't worry this won't affect your ranking.\n",
		player.Name,
	)

	b.notifications <- notif
}

func (b *Back) sendMatchSessionEmptyNotification(tx *sqlx.Tx, session MatchSession) error {
	league, err := getLeagueByID(tx, session.LeagueID)
	if err != nil {
		return err
	}

	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordChannel,
		Recipient:     league.AnnounceDiscordChannelID,
		Type:          NotificationTypeMatchSessionEmpty,
	}

	notif.Printf(
		"The race for league `%s` is closed, you can no longer join.\n"+
			"There was not enough players to start the race.\n",
		league.ShortCode,
	)

	b.notifications <- notif
	return nil
}

func (b *Back) sendMatchEndNotification(
	tx *sqlx.Tx,
	match Match,
	selfEntry MatchEntry,
	opponentEntry MatchEntry,
	player Player,
) error {
	opponent, err := getPlayerByID(tx, opponentEntry.PlayerID)
	if err != nil {
		return err
	}

	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordUser,
		Recipient:     player.DiscordID.String,
		Type:          NotificationTypeMatchEnd,
	}

	notif.Printf("%s, your race against %s has ended.\n", player.Name, opponent.Name)

	start, end := selfEntry.StartedAt.Time.Time(), selfEntry.EndedAt.Time.Time()
	delta := end.Sub(start).Round(time.Second)
	if selfEntry.Status == MatchEntryStatusForfeit {
		notif.Printf("You forfeited your race after %s.\n", delta)
	} else if selfEntry.Status == MatchEntryStatusFinished {
		notif.Printf("You completed your race in %s.\n", delta)
	}

	start, end = opponentEntry.StartedAt.Time.Time(), opponentEntry.EndedAt.Time.Time()
	delta = end.Sub(start).Round(time.Second)
	if opponentEntry.Status == MatchEntryStatusForfeit {
		notif.Printf("%s forfeited after %s.\n", opponent.Name, delta)
	} else if opponentEntry.Status == MatchEntryStatusFinished {
		notif.Printf("%s completed his/her race in %s.\n", opponent.Name, delta)
	}

	switch selfEntry.Outcome {
	case MatchEntryOutcomeWin:
		notif.Print("**You won!**")
	case MatchEntryOutcomeDraw:
		notif.Print("**The race is a draw.**")
	case MatchEntryOutcomeLoss:
		notif.Printf("**%s wins.**", opponent.Name)
	}

	b.notifications <- notif
	return nil
}

func (b *Back) sendMatchSeedNotification(
	session MatchSession,
	patch []byte,
	p1, p2 Player,
) {
	name := fmt.Sprintf(
		"seed_%s.zpf",
		session.StartDate.Time().Format("2006-01-02_15h04"),
	)

	send := func(player Player) {
		notif := Notification{
			RecipientType: NotificationRecipientTypeDiscordUser,
			Recipient:     player.DiscordID.String,
			Type:          NotificationTypeMatchSeed,
			Files: []NotificationFile{{
				Name:        name,
				ContentType: "application/zlib",
				Reader:      bytes.NewReader(patch),
			}},
		}

		notif.Printf("Here is your seed in _Patch_ format. "+
			"You can use https://ootrandomizer.com/generator to patch your ROM.\n"+
			"Your race starts in %s, **do not explore the seed before the match starts**.",
			time.Until(session.StartDate.Time()).Round(time.Second),
		)

		b.notifications <- notif
	}

	send(p1)
	send(p2)
}

func (b *Back) sendSessionStatusUpdateNotification(tx *sqlx.Tx, session MatchSession) error {
	league, err := getLeagueByID(tx, session.LeagueID)
	if err != nil {
		return err
	}

	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordChannel,
		Recipient:     league.AnnounceDiscordChannelID,
		Type:          NotificationTypeMatchSessionStatusUpdate,
	}

	switch session.Status {
	case MatchSessionStatusWaiting:
		notif.Printf(
			"The next race for league `%s` has been scheduled for %s (in %s)",
			league.ShortCode,
			session.StartDate.Time(),
			time.Until(session.StartDate.Time()).Round(time.Second),
		)
	case MatchSessionStatusJoinable:
		notif.Printf(
			"The race for league `%s` can now be joined! The race starts at %s (in %s).\n"+
				"You can join using `!join %s`.",
			league.ShortCode,
			session.StartDate.Time(),
			time.Until(session.StartDate.Time()).Round(time.Second),
			league.ShortCode,
		)
	case MatchSessionStatusPreparing:
		notif.Printf(
			"The race for league `%s` has begun preparations, you can no longer join. "+
				"Seeds will soon be sent to the %d contestants.\n"+
				"The race starts at %s (in %s). Watch this channel for the official go.",
			league.ShortCode,
			len(session.PlayerIDs)-(len(session.PlayerIDs)%2),
			session.StartDate.Time(),
			time.Until(session.StartDate.Time()).Round(time.Second),
		)
	case MatchSessionStatusInProgress:
		notif.Printf(
			"The race for league `%s` **starts now**. Good luck and have fun! @here",
			league.ShortCode,
		)
	case MatchSessionStatusClosed:
		notif.Printf(
			"All players have finished their last `%s` race, rankings have been updated.",
			league.ShortCode,
		)
	}

	b.notifications <- notif
	return nil
}

func (b *Back) sendSessionCountdownNotification(tx *sqlx.Tx, session MatchSession) error {
	league, err := getLeagueByID(tx, session.LeagueID)
	if err != nil {
		return err
	}

	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordChannel,
		Recipient:     league.AnnounceDiscordChannelID,
		Type:          NotificationTypeMatchSessionStatusUpdate,
	}

	delta := time.Until(session.StartDate.Time()).Round(time.Second)
	notif.Printf(
		"The next race for league `%s` starts in %s.",
		league.ShortCode, delta,
	)

	switch delta {
	case time.Minute, 30 * time.Second, 5 * time.Second:
		notif.Printf(" @here")
	default:
	}

	b.notifications <- notif
	return nil
}

func (b *Back) sendSessionRecapNotification(
	tx *sqlx.Tx,
	session MatchSession,
	matches []Match,
) error {
	league, err := getLeagueByID(tx, session.LeagueID)
	if err != nil {
		return err
	}

	notif := Notification{
		RecipientType: NotificationRecipientTypeDiscordChannel,
		Recipient:     league.AnnounceDiscordChannelID,
		Type:          NotificationTypeMatchSessionStatusUpdate,
	}

	notif.Printf("Results for latest `%s` race:\n```\n", league.ShortCode)
	table := tabwriter.NewWriter(&notif, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "Player 1\t\tvs\tPlayer 2\t\tSeed")

	details := func(entry MatchEntry) (wrap string, name string, duration string) {
		if entry.Outcome == MatchEntryOutcomeWin {
			wrap = "*"
		}

		player, _ := getPlayerByID(tx, entry.PlayerID)
		name = player.Name

		if entry.Status == MatchEntryStatusForfeit {
			duration = "forfeit"
			return
		}

		delta := entry.EndedAt.Time.Time().Sub(entry.StartedAt.Time.Time()).Round(time.Second)
		duration = delta.String()

		return
	}

	for _, match := range matches {
		wrap0, name0, duration0 := details(match.Entries[0])
		wrap1, name1, duration1 := details(match.Entries[1])

		fmt.Fprint(table,
			wrap0, name0, wrap0, "\t", duration0, "\t\t",
			wrap1, name1, wrap1, "\t", duration1, "\t", match.Seed, "\n",
		)
	}

	table.Flush()
	notif.Print("```\n")

	b.notifications <- notif
	return nil
}
