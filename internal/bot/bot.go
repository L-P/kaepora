// Package bot handles the Discord bot that talks to the back.Back.
// If a bot for another messaging platform is added, it should live in a
// separate module and this one should be renamed.
package bot

import (
	"errors"
	"fmt"
	"io"
	"kaepora/internal/back"
	"kaepora/internal/config"
	"kaepora/internal/util"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type commandHandler func(m *discordgo.Message, args []string, w io.Writer) error

// Bot is a Discord bot that acts as a CLI front-end for the Back.
type Bot struct {
	back   *back.Back
	config *config.Config

	manualSeedgenLimiter *rate.Limiter

	startedAt time.Time
	dg        *discordgo.Session

	handlers      map[string]commandHandler
	notifications <-chan back.Notification
}

// New creates a new Discord bot ready to be launched with Serve.
func New(back *back.Back, config *config.Config) (*Bot, error) {
	dg, err := discordgo.New("Bot " + config.Discord.Token)
	if err != nil {
		return nil, err
	}

	bot := &Bot{
		back:                 back,
		config:               config,
		dg:                   dg,
		startedAt:            time.Now(),
		notifications:        back.GetNotificationsChan(),
		manualSeedgenLimiter: rate.NewLimiter(4.0/60.0, 1), // allow four seeds / minute
	}

	dg.AddHandler(bot.handleMessage)

	bot.handlers = map[string]commandHandler{
		"!dev": bot.cmdDev,

		"!help":         bot.cmdHelp,
		"!leaderboard":  bot.cmdLeaderboards,
		"!leaderboards": bot.cmdLeaderboards,
		"!leagues":      bot.cmdLeagues,
		"!no":           bot.cmdHelp,
		"!recap":        bot.cmdRecap,
		"!register":     bot.cmdRegister,
		"!rename":       bot.cmdRename,
		"!seed":         bot.cmdSendSeed,
		"!setstream":    bot.cmdSetStream,
		"!yes":          bot.cmdAllRight,

		"!cancel":   bot.cmdCancel,
		"!unjoin":   bot.cmdCancel,
		"!complete": bot.cmdComplete,
		"!done":     bot.cmdComplete,
		"!forfeit":  bot.cmdForfeit,
		"!join":     bot.cmdJoin,
	}

	return bot, nil
}

// Serve runs the Discord bot until the done channel is closed.
func (bot *Bot) Serve(wg *sync.WaitGroup, done <-chan struct{}) {
	log.Println("info: starting Discord bot")
	wg.Add(1)
	defer wg.Done()

	if !bot.config.Discord.CanRunBot() {
		log.Println("warning: Discord bot is not configured, not running it")
		bot.idle(done)
		return
	}

	if err := bot.dg.Open(); err != nil {
		log.Panic(err)
	}

loop:
	for {
		select {
		case notif := <-bot.notifications:
			if err := bot.sendNotification(notif); err != nil {
				log.Printf("unable to send notification: %s", err)
			}
		case <-done:
			break loop
		}
	}

	if err := bot.dg.Close(); err != nil {
		log.Printf("error: could not close Discord bot: %s", err)
	}
	log.Println("info: Discord bot closed")
}

// idle does nothing until done is closed.
// It consumes notifications and log them.
func (bot *Bot) idle(done <-chan struct{}) {
loop:
	for {
		select {
		case notif := <-bot.notifications:
			log.Printf("info: not sent: %s", notif.String())
		case <-done:
			break loop
		}
	}
}

// isListeningOn returns true if the bot should listen to commands sent on the given channel ID.
func (bot *Bot) isListeningOn(channelID string) bool {
	// This should be cached into a map, but I don't plan on having more than
	// one or two channels for now.
	for _, v := range bot.config.Discord.ListenIDs {
		if channelID == v {
			return true
		}
	}

	return false
}

// handleMessage treats incoming messages as CLI commands and runs the corresponding back code.
func (bot *Bot) handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore webooks, self, bots.
	if m.Author == nil || m.Author.ID == s.State.User.ID || m.Author.Bot {
		return
	}

	channel, err := s.Channel(m.ChannelID)
	if err != nil {
		log.Printf("error: unable to fetch channel: %s", err)
		return
	}

	// Only act on PMs and a predetermined set of channels.
	if channel.Type == discordgo.ChannelTypeGuildText {
		// Let the command that adds a channel through.
		bypass := m.Message.Content == `!dev addlisten` && bot.config.IsDiscordIDAdmin(m.Author.ID)
		if !bypass && !bot.isListeningOn(m.ChannelID) {
			return
		}

		if err := s.ChannelMessageDelete(m.ChannelID, m.Message.ID); err != nil {
			log.Printf("error: unable to delete message: %s", err)
		}
	}

	log.Printf(
		"info: <%s(%s)@%s#%s> %s",
		m.Author.String(), m.Author.ID,
		m.GuildID, m.ChannelID,
		m.Content,
	)

	bot.createWriterAndDispatch(s, m.Message, m.Author.ID)
}

func (bot *Bot) createWriterAndDispatch(s *discordgo.Session, m *discordgo.Message, recipientID string) {
	// Ignore non-commands.
	if !strings.HasPrefix(m.Content, "!") {
		return
	}

	out, err := newUserChannelWriter(s, recipientID)
	if err != nil {
		log.Printf("error: could not create channel writer: %s", err)
	}
	defer func() {
		if err := out.Flush(); err != nil {
			log.Printf("error: could not send message: %s", err)
		}
	}()

	if err := bot.dispatch(m, out); err != nil {
		out.Reset()
		fmt.Fprintln(out, "There was an error processing your command.")

		if errors.Is(err, util.ErrPublic("")) || bot.config.IsDiscordIDAdmin(recipientID) {
			fmt.Fprintf(out, "```%s\n```\nIf you need help, send `!help`.", err)
		}

		log.Printf("error: failed to process command: %s", err)
	}
}

// dispatch is the handleMessage internals without the Discord-specific stuff.
func (bot *Bot) dispatch(m *discordgo.Message, w *channelWriter) error {
	defer func() {
		r := recover()
		if r != nil {
			w.Reset()
			fmt.Fprint(w, "Someting went very wrong.")
			log.Print("panic: ", r)
			log.Printf("%s", debug.Stack())
		}
	}()

	if bot.config.IsDiscordIDBanned(m.Author.ID) {
		return util.ErrPublic("your account has been banned")
	}

	command, args := parseCommand(m.Content)
	handler, ok := bot.handlers[strings.ToLower(command)]
	if !ok {
		return util.ErrPublic(fmt.Sprintf("invalid command: %v", m.Content))
	}

	return handler(m, args, w)
}

// parseCommand splits a raw string as sent to the bot into a command name and
// its space-separated arguments.
func parseCommand(cmd string) (string, []string) {
	parts := strings.Split(cmd, " ")

	switch len(parts) {
	case 0:
		return "", nil
	case 1:
		return parts[0], nil
	default:
		return parts[0], parts[1:]
	}
}

func (bot *Bot) cmdHelp(m *discordgo.Message, _ []string, w io.Writer) error {
	fmt.Fprintf(w, "Hoo hoot! %s… Look up here!\n"+
		"It appears that the time has finally come for you to start your adventure!\n"+
		"You will encounter many hardships ahead… That is your fate.\n"+
		"Don't feel discouraged, even during the toughest times!\n\n",
		m.Author.Mention(),
	)

	// nolint:lll
	fmt.Fprintf(w, `**Available commands**:

Brackets indicate optional arguments.

%[1]s
# Management
!help                   # display this help message
!leaderboard SHORTCODE  # show leaderboards for the given league
!leagues                # list leagues
!recap [SHORTCODE]      # show the 1v1 results for the current session
!register [NAME]        # create your account and link it to your Discord account
!rename NAME            # set your display name to NAME
!setstream URL          # set your stream URL

!seed SHORTCODE [VERSION] [SEED]  # generate a seed valid for the given league
                                  # VERSION must be a valid OOTR version number

# Racing
!cancel            # cancel joining the next race without penalty until T%[3]s
!done              # stop your race timer and register your final time
!forfeit           # forfeit (and thus lose) the current race
!join SHORTCODE    # join the next race of the given league (see !leagues)
%[1]s

**Racing**:
You can freely join a race and cancel without consequences between T%[2]s and T%[3]s.
When the race reaches its preparation phase at T%[3]s you can no longer cancel and must either complete or forfeit the race.
You can't join a race that is in progress or has begun its preparation phase (T%[3]s).
If you are caught cheating, using an alt, or breaking a league's rules **you will be banned**.

Did you get all that?
`,
		"```",
		util.FormatDuration(back.MatchSessionJoinableAfterOffset),
		util.FormatDuration(back.MatchSessionPreparationOffset),
	)

	return nil
}

func argsAsName(args []string) string {
	return strings.Trim(strings.Join(args, " "), "  \t\n")
}

func (bot *Bot) cmdAllRight(m *discordgo.Message, _ []string, w io.Writer) error {
	fmt.Fprintf(w, "All right then, I'll see you around!\nHoot hoot hoot ho!")
	return nil
}

func (bot *Bot) cmdSendSeed(m *discordgo.Message, args []string, w io.Writer) error {
	if len(args) < 1 || len(args) > 3 {
		return util.ErrPublic("expected 1 to 3 arguments: SHORTCODE [VERSION] [SEED]")
	}

	if !bot.manualSeedgenLimiter.Allow() {
		fmt.Fprintf(w, "Too many seeds are being generated right now\nTry again in 20 seconds.")
		return nil
	}

	var version string
	if len(args) > 1 {
		version = args[1]
	}

	seed := uuid.New().String()
	if len(args) > 2 {
		seed = args[2]
	}

	return bot.back.SendDevSeed(m.Author.ID, args[0], seed, version)
}
