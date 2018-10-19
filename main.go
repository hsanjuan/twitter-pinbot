package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	cid "github.com/ipfs/go-cid"
	"github.com/ipfs/ipfs-cluster/api"
	"github.com/ipfs/ipfs-cluster/api/rest/client"
	multiaddr "github.com/multiformats/go-multiaddr"

	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
)

// ConfigFile is the path of the default configuration file
var ConfigFile = "config.json"

// Gateway
var IPFSGateway = "https://ipfs.io"

type Action string

// Variables containing the different available actions
var (
	// (spaces)(action)whitespaces(arguments)
	actionRegexp = regexp.MustCompile(`^\s*([[:graph:]]+)\s+(.+)$`)
	// (cid)whitespaces(name)
	pinRegexp          = regexp.MustCompile(`([[:graph:]]+)\s+([[:graph:]]+)$`)
	PinAction   Action = "!pin"
	UnpinAction Action = "!unpin"
	AddAction   Action = "!add"
	HelpAction  Action = "!help"
)

func (a Action) Valid() bool {
	switch a {
	case PinAction, UnpinAction, AddAction, HelpAction:
		return true
	}
	return false
}

func (a Action) String() string {
	return string(a)
}

// Config is the configuration format for the Twitter Pinbot
type Config struct {
	TwitterID       string `json:"twitter_id"`
	TwitterName     string `json:"twitter_name"`
	AccessKey       string `json:"access_key"`
	AccessSecret    string `json:"access_secret"`
	ConsumerKey     string `json:"consumer_key"`
	ConsumerSecret  string `json:"consumer_secret"`
	ClusterPeerAddr string `json:"cluster_peer_addr"`
	ClusterUsername string `json:"cluster_username"`
	ClusterPassword string `json:"cluster_password"`
}

func readConfig(path string) *Config {
	cfg := &Config{}
	cfgFile, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	err = json.Unmarshal(cfgFile, &cfg)
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

// Bot is a twitter bot which reads a user's timeline
// and performs actions on IPFS Cluster if the tweets
// match, i.e. a tweet with: "@bothandle pin <cid> name"
// will pin something. The users with pin permissions are
// those followed by the bot. Retweets by users followed
// by the bot should also work. The bot will answer
// the tweet with a result.
type Bot struct {
	ctx    context.Context
	cancel context.CancelFunc

	name          string
	id            string
	twClient      *twitter.Client
	clusterClient client.Client

	follows sync.Map

	die chan struct{}
}

// New creates a new Bot with the Config.
func New(cfg *Config) (*Bot, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Twitter client
	ocfg := oauth1.NewConfig(cfg.ConsumerKey, cfg.ConsumerSecret)
	token := oauth1.NewToken(cfg.AccessKey, cfg.AccessSecret)
	httpClient := ocfg.Client(ctx, token)
	twClient := twitter.NewClient(httpClient)

	// IPFS Cluster client
	peerAddr, err := multiaddr.NewMultiaddr(cfg.ClusterPeerAddr)
	if err != nil {
		cancel()
		return nil, err
	}
	clusterClient, err := client.NewDefaultClient(&client.Config{
		APIAddr:  peerAddr,
		Username: cfg.ClusterUsername,
		Password: cfg.ClusterPassword,
		LogLevel: "debug",
	})

	if err != nil {
		cancel()
		return nil, err
	}

	bot := &Bot{
		ctx:           ctx,
		cancel:        cancel,
		twClient:      twClient,
		clusterClient: clusterClient,
		name:          cfg.TwitterName,
		id:            cfg.TwitterID,
		die:           make(chan struct{}, 1),
	}

	bot.fetchFollowing()
	go bot.watchFollowing()
	go bot.watchTweets()
	return bot, nil
}

// Kill destroys this bot.
func (b *Bot) Kill() {
	b.cancel()
}

// Name returns the twitter handle used by the bot
func (b *Bot) Name() string {
	return b.name
}

// ID returns the twitter user ID used by the bot
func (b *Bot) ID() string {
	return b.id
}

func (b *Bot) fetchFollowing() {
	var nextCursor int64 = -1
	includeEntities := false
	for nextCursor != 0 {
		following, _, err := b.twClient.Friends.List(
			&twitter.FriendListParams{
				Count:               200,
				IncludeUserEntities: &includeEntities,
			})
		if err != nil {
			log.Println(err)
		}
		for _, u := range following.Users {
			log.Println("following:", u.ScreenName)
			b.follows.Store(u.ID, struct{}{})
		}
		nextCursor = following.NextCursor
	}
}

func (b *Bot) watchFollowing() {
	for {
		time.Sleep(90 * time.Second)
		select {
		case <-b.ctx.Done():
		default:
			b.fetchFollowing()
		}
	}
}

func (b *Bot) parseTweet(tweet *twitter.Tweet) (Action, string, []string, error) {
	// remote our username if they started with it
	text := strings.TrimPrefix(tweet.Text, b.name)
	var action Action
	var arguments string

	// match to see if any action
	matches := actionRegexp.FindAllStringSubmatch(text, -1)
	if len(matches) > 0 {
		firstMatch := matches[0]
		action = Action(firstMatch[1]) // first group match
		arguments = firstMatch[2]      // second group match
	}

	urls := extractMediaURLs(tweet)
	return action, arguments, urls, nil
}

func extractMediaURLs(tweet *twitter.Tweet) []string {
	var urls []string
	// Grab any media entities from the tweet
	if tweet.ExtendedEntities != nil && tweet.ExtendedEntities.Media != nil {
		for _, media := range tweet.ExtendedEntities.Media {
			urls = append(urls, extractMediaURL(&media))
		}
	}
	return urls

}

type byBitrate []twitter.VideoVariant

func (vv byBitrate) Len() int           { return len(vv) }
func (vv byBitrate) Swap(i, j int)      { vv[i], vv[j] = vv[j], vv[i] }
func (vv byBitrate) Less(i, j int) bool { return vv[i].Bitrate < vv[j].Bitrate }

func extractMediaURL(me *twitter.MediaEntity) string {
	switch me.Type {
	case "video", "animated_gif":
		variants := me.VideoInfo.Variants
		sort.Sort(byBitrate(variants))
		// pick video with highest bitrate
		last := variants[len(variants)-1]
		return last.URL
	default:
		return me.MediaURL
	}
}

func (b *Bot) watchTweets() {
	log.Println("watching tweets")

	params := &twitter.StreamFilterParams{
		Follow: []string{b.id},
		Track: []string{
			PinAction.String(),
			UnpinAction.String(),
			HelpAction.String(),
			AddAction.String(),
		},
		StallWarnings: twitter.Bool(true),
	}

	stream, err := b.twClient.Streams.Filter(params)
	if err != nil {
		log.Println(err)
	}

	demux := twitter.NewSwitchDemux()
	demux.Tweet = b.processTweet
	for {
		select {
		case <-b.ctx.Done():
			return
		case msg := <-stream.Messages:
			//log.Println("handling message", msg)
			go demux.Handle(msg)
		}
	}
}

func (b *Bot) processTweet(tweet *twitter.Tweet) {
	log.Printf("Parsing %+v\n", tweet.Text)
	//log.Printf("%+v\n", tweet.User)

	action, arguments, urls, err := b.parseTweet(tweet)
	if err != nil {
		b.tweet(err.Error(), tweet)
		return
	}

	log.Printf("Parsed: %s, %s, %s\n", action, arguments, urls)

	_, ok := b.follows.Load(tweet.User.ID)
	if !ok && action.Valid() {
		b.tweet("Sorry but I don't follow you yet", tweet)
		return
	}
	// We follow the user. We do stuff.

	// Process actions
	switch action {
	case PinAction:
		b.pin(arguments, tweet)
	case UnpinAction:
		b.unpin(arguments, tweet)
	case AddAction:
		b.add(arguments, tweet)
	case HelpAction:
		b.tweetHelp(tweet)
	default:
		log.Println("no handled action for this tweet")
	}

	// Add any media urls
	if len(urls) > 0 {
		log.Println("adding media: ", urls)
		out := make(chan *api.AddedOutput, 1)
		go func() {
			cids := []string{}
			for added := range out {
				log.Printf("added %s\n", added.Hash)
				cids = append(cids, added.Hash)
			}
			if len(cids) > 0 {
				b.tweetAdded(cids, tweet)
			}
		}()
		params := api.DefaultAddParams()
		params.Wrap = true
		params.Name = "Tweet-" + tweet.IDStr
		err := b.clusterClient.Add(urls, params, out)
		if err != nil {
			log.Println(err)
		}
	}

	if quote := tweet.QuotedStatus; quote != nil {
		// process the QuotedStatus as if it was from original user
		quote.User.ID = tweet.User.ID
		quote.User.ScreenName = tweet.User.ScreenName
		quote.ID = tweet.ID
		b.processTweet(quote)
	}
}

func (b *Bot) pin(args string, tweet *twitter.Tweet) {
	log.Println("pin with ", args)
	pinUsage := fmt.Sprintf("Usage: '%s <cid> <name>'", PinAction)

	matches := pinRegexp.FindAllStringSubmatch(args, -1)
	if len(matches) == 0 {
		b.tweet(pinUsage, tweet)
		return
	}

	firstMatch := matches[0]
	cidStr := firstMatch[1]
	name := firstMatch[2]
	c, err := cid.Decode(cidStr)
	if err != nil {
		b.tweet(pinUsage+". Make sure your CID is valid", tweet)
		return
	}

	err = b.clusterClient.Pin(c, 0, 0, name)
	if err != nil {
		log.Println(err)
		b.tweet("An error happened pinning. I will re-start myself. Please retry in a bit", tweet)
		b.die <- struct{}{}
		return
	}
	waitParams := client.StatusFilterParams{
		Cid:       c,
		Local:     false,
		Target:    api.TrackerStatusPinned,
		CheckFreq: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Minute)
	defer cancel()
	_, err = client.WaitFor(ctx, b.clusterClient, waitParams)
	if err != nil {
		log.Println(err)
		b.tweet("IPFS Cluster did not manage to pin the item, but it's tracking it", tweet)
		return
	}

	b.tweet(fmt.Sprintf("Pinned! Check it out at %s/ipfs/%s", IPFSGateway, cidStr), tweet)
}

func (b *Bot) unpin(args string, tweet *twitter.Tweet) {
	log.Println("unpin with ", args)
	unpinUsage := fmt.Sprintf("Usage: '%s <cid>'", UnpinAction)

	c, err := cid.Decode(args)
	if err != nil {
		b.tweet(unpinUsage+". Make sure your CID is valid", tweet)
		return
	}

	err = b.clusterClient.Unpin(c)
	if err != nil && !strings.Contains(err.Error(), "uncommited to state") {
		log.Println(err)
		b.tweet("An error happened unpinning. I will re-start myself. Please retry in a bit", tweet)
		b.die <- struct{}{}
		return
	}
	waitParams := client.StatusFilterParams{
		Cid:       c,
		Local:     false,
		Target:    api.TrackerStatusUnpinned,
		CheckFreq: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(b.ctx, time.Minute)
	defer cancel()
	_, err = client.WaitFor(ctx, b.clusterClient, waitParams)
	if err != nil {
		log.Println(err)
		b.tweet("IPFS Cluster did not manage to unpin the item, but it's trying...", tweet)
		return
	}

	b.tweet(fmt.Sprintf("Unpinned %s! :'(", args), tweet)
}

func (b *Bot) add(arg string, tweet *twitter.Tweet) {
	log.Println("add with ", arg)
	addUsage := fmt.Sprintf("Usage: '%s <http-or-https-url>'")
	url, err := url.Parse(arg)
	if err != nil {
		b.tweet(addUsage+". Make sure you gave a valid url!", tweet)
		return
	}
	if url.Scheme != "http" && url.Scheme != "https" {
		b.tweet(addUsage+". Not an HTTP(s) url!", tweet)
		return
	}

	if url.Host == "localhost" || url.Host == "127.0.0.1" || url.Host == "::1" {
		b.tweet("ehem ehem...", tweet)
		return
	}

	out := make(chan *api.AddedOutput, 1)
	go func() {
		cids := []string{}
		for added := range out {
			cids = append(cids, added.Hash)
		}
		if len(cids) > 0 {
			b.tweetAdded(cids, tweet)
		}
	}()

	params := api.DefaultAddParams()
	params.Wrap = false
	params.Name = "Tweet-" + tweet.IDStr
	err = b.clusterClient.Add([]string{arg}, params, out)
	if err != nil {
		log.Println(err)
		b.tweet("An error happened adding. I will re-start myself. Please retry in a bit", tweet)
		b.die <- struct{}{}
		return
	}
}

func (b *Bot) tweetAdded(cids []string, tweet *twitter.Tweet) {
	msg := "Your content has been added to #IPFS Cluster!\n"
	msg += "Check it out:\n\n"
	for _, c := range cids {
		msg += fmt.Sprintf(" > %s/ipfs/%s\n", IPFSGateway, c)
	}
	b.tweet(msg, tweet)
}

func (b *Bot) tweetHelp(tweet *twitter.Tweet) {
	help := fmt.Sprintf(`Hi! Currently I support:

!pin <cid> name
!unpin <cid>
!add <url-to-single-file>
!help

Happy pinning!
`, b.name)
	b.tweet(help, tweet)
}

func (b *Bot) tweet(msg string, origTweet *twitter.Tweet) {
	var err error
	var tweetMsg string = msg
	var params *twitter.StatusUpdateParams

	if origTweet != nil {
		params = &twitter.StatusUpdateParams{
			InReplyToStatusID: origTweet.ID,
		}
		tweetMsg = fmt.Sprintf("@%s %s", origTweet.User.ScreenName, msg)
	}
	log.Println("tweeting: ", tweetMsg)

	_, _, err = b.twClient.Statuses.Update(tweetMsg, params)
	if err != nil {
		log.Println(err)
	}
	return
}

func main() {
	path := flag.String("config", ConfigFile, "path to config file")
	flag.Parse()

	cfg := readConfig(*path)

	bot, err := New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Bot created:", bot.Name(), bot.ID())

	// Wait for SIGINT and SIGTERM (HIT CTRL-C)
	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-ch:
		log.Println(sig)
	case <-bot.die:
	}

	bot.Kill()
}
