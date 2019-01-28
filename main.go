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

const twittercom = "twitter.com"

type Action string

// Variables containing the different available actions
var (
	// (spaces)(action)whitespaces(arguments)
	actionRegexp = regexp.MustCompile(`^\s*([[:graph:]]+)\s+(.+)`)
	// (cid)whitespaces(name with whitespaces). [:graph:] does not
	// match line breaks or spaces.
	pinRegexp          = regexp.MustCompile(`([[:graph:]]+)\s+([[:graph:]\s]+)`)
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
		LogLevel: "info",
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
			_, old := b.follows.LoadOrStore(u.ID, struct{}{})
			if !old {
				log.Println("Friend: ", u.ScreenName)
			}
		}
		nextCursor = following.NextCursor
		time.Sleep(2 * time.Second)
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

func (b *Bot) processTweet(tweet *twitter.Tweet, srcTweet *twitter.Tweet) {
	if tweet == nil {
		return
	}

	if srcTweet == nil {
		srcTweet = tweet
	}

	// Skip processing our own tweets (written by us)
	// and quotes or retweets we've made (origUser is us)
	// (avoid potential loops)
	if tweet.User.IDStr == b.ID() || srcTweet.User.IDStr == b.ID() {
		return
	}

	action, arguments, urls, err := b.parseTweet(tweet)
	if err != nil {
		b.tweet(err.Error(), tweet, srcTweet, false)
		return
	}

	log.Printf("Parsed: %s, %s, %s\n", action, arguments, urls)

	_, ok := b.follows.Load(srcTweet.User.ID)
	if !ok && action.Valid() {
		b.tweet("Sorry but I don't follow you yet", tweet, srcTweet, false)
		return
	}
	if !ok {
		return
	}
	// We follow the user. We do stuff.

	// Process actions
	switch action {
	case PinAction:
		b.pin(arguments, tweet, srcTweet)
	case UnpinAction:
		b.unpin(arguments, tweet, srcTweet)
	case AddAction:
		b.add(arguments, tweet, srcTweet)
	case HelpAction:
		b.tweetHelp(tweet, srcTweet)
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
				log.Printf("added %s\n", added.Cid)
				cids = append(cids, added.Cid)
			}
			if len(cids) > 0 {
				b.tweetAdded(cids, tweet, srcTweet)
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

	// If the tweet has retweets, process them as if they were
	// from this user.
	retweets := []*twitter.Tweet{tweet.QuotedStatus, tweet.RetweetedStatus}
	for _, rt := range retweets {
		b.processTweet(rt, srcTweet)
	}
}

// parseTweet returns Action, arguments, media urls, and error
func (b *Bot) parseTweet(tweet *twitter.Tweet) (Action, string, []string, error) {
	// Extended tweet? let's use the entities from the extended tweet then.
	if tweet.ExtendedTweet != nil {
		tweet.Entities = tweet.ExtendedTweet.Entities
		tweet.ExtendedEntities = tweet.ExtendedTweet.ExtendedEntities
		tweet.FullText = tweet.ExtendedTweet.FullText

	}
	text := tweet.FullText
	if text == "" {
		text = tweet.Text
	}

	log.Println("Parsing:", text)

	// remote our username if they started with it
	text = strings.TrimPrefix(text, b.name)
	var action Action
	var arguments string

	if text == " "+string(HelpAction) {
		return HelpAction, "", []string{}, nil
	}

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

func tweetFile(tweet *twitter.Tweet) {

}

// takes *Entities or *MediaEntities
func media(ent interface{}) []twitter.MediaEntity {
	if ent == nil {
		return nil
	}

	switch ent.(type) {
	case *twitter.Entities:
		e := ent.(*twitter.Entities)
		if e != nil {
			return e.Media
		}
	case *twitter.ExtendedEntity:
		e := ent.(*twitter.ExtendedEntity)
		if e != nil {
			return e.Media
		}
	}
	return nil
}

func extractMediaURLs(tweet *twitter.Tweet) []string {
	var urls []string

	// Grab any media entities from the tweet
	for _, m := range media(tweet.ExtendedEntities) {
		urls = append(urls, extractMediaURL(&m))
	}

	if len(urls) == 0 {
		// If no extended entitites, try with traditional.
		for _, m := range media(tweet.Entities) {
			urls = append(urls, extractMediaURL(&m))
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
			b.Name(),
		},
		StallWarnings: twitter.Bool(true),
	}

	stream, err := b.twClient.Streams.Filter(params)
	if err != nil {
		log.Println(err)
	}

	demux := twitter.NewSwitchDemux()
	demux.Tweet = func(t *twitter.Tweet) {
		b.processTweet(t, t)
	}
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

func (b *Bot) pin(args string, tweet, srcTweet *twitter.Tweet) {
	log.Println("pin with ", args)
	pinUsage := fmt.Sprintf("Usage: '%s <cid> <name>'", PinAction)

	matches := pinRegexp.FindAllStringSubmatch(args, -1)
	if len(matches) == 0 {
		b.tweet(pinUsage, srcTweet, nil, false)
		return
	}

	firstMatch := matches[0]
	cidStr := firstMatch[1]
	name := firstMatch[2]
	c, err := cid.Decode(cidStr)
	if err != nil {
		b.tweet(pinUsage+". Make sure your CID is valid.", tweet, srcTweet, false)
		return
	}

	err = b.clusterClient.Pin(c, 0, 0, name)
	if err != nil {
		log.Println(err)
		b.tweet("An error happened pinning. I will re-start myself. Please retry in a bit.", srcTweet, nil, false)
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
		b.tweet("IPFS Cluster did not manage to pin the item, but it's tracking it.", srcTweet, nil, false)
		return
	}

	b.tweet(fmt.Sprintf("Pinned! Check it out at %s/ipfs/%s", IPFSGateway, cidStr), tweet, srcTweet, true)
}

func (b *Bot) unpin(args string, tweet, srcTweet *twitter.Tweet) {
	log.Println("unpin with ", args)
	unpinUsage := fmt.Sprintf("Usage: '%s <cid>'", UnpinAction)

	c, err := cid.Decode(args)
	if err != nil {
		b.tweet(unpinUsage+". Make sure your CID is valid.", tweet, srcTweet, false)
		return
	}

	err = b.clusterClient.Unpin(c)
	if err != nil && !strings.Contains(err.Error(), "uncommited to state") {
		log.Println(err)
		b.tweet("An error happened unpinning. I will re-start myself. Please retry in a bit.", srcTweet, nil, false)
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
		b.tweet("IPFS Cluster did not manage to unpin the item, but it's trying...", srcTweet, nil, false)
		return
	}

	b.tweet(fmt.Sprintf("Unpinned %s! :'(", args), tweet, srcTweet, false)
}

func (b *Bot) add(arg string, tweet, srcTweet *twitter.Tweet) {
	log.Println("add with ", arg)
	addUsage := fmt.Sprintf("Usage: '%s <http-or-https-url>'")
	url, err := url.Parse(arg)
	if err != nil {
		b.tweet(addUsage+". Make sure you gave a valid url!", srcTweet, nil, false)
		return
	}
	if url.Scheme != "http" && url.Scheme != "https" {
		b.tweet(addUsage+". Not an HTTP(s) url!", srcTweet, nil, false)
		return
	}

	if url.Host == "localhost" || url.Host == "127.0.0.1" || url.Host == "::1" {
		b.tweet("ehem ehem...", srcTweet, nil, false)
		return
	}

	out := make(chan *api.AddedOutput, 1)
	go func() {
		cids := []string{}
		for added := range out {
			cids = append(cids, added.Cid)
		}
		if len(cids) > 0 {
			b.tweetAdded(cids, tweet, srcTweet)
		}
	}()

	params := api.DefaultAddParams()
	params.Wrap = true
	params.Name = "Tweet-" + tweet.IDStr
	err = b.clusterClient.Add([]string{arg}, params, out)
	if err != nil {
		log.Println(err)
		b.tweet("An error happened adding. I will re-start myself. Please retry in a bit.", srcTweet, nil, false)
		b.die <- struct{}{}
		return
	}
}

func (b *Bot) tweetAdded(cids []string, tweet, srcTweet *twitter.Tweet) {
	msg := "Just added this to #IPFS Cluster!\n\n"
	for i, c := range cids {
		if i != len(cids)-1 {
			msg += fmt.Sprintf("• File: %s/ipfs/%s\n", IPFSGateway, c)
		} else { // last
			msg += fmt.Sprintf("• Folder-wrap: %s/ipfs/%s\n", IPFSGateway, c)
		}
	}
	b.tweet(msg, tweet, srcTweet, true)
}

func (b *Bot) tweetHelp(tweet, srcTweet *twitter.Tweet) {
	help := fmt.Sprintf(`Hi! Here's what I can do:

!pin <cid> <name>
!unpin <cid>
!add <url-to-single-file>
!help

You can always prepend these commands mentioning me (%s).

Happy pinning!
`, b.name)
	b.tweet(help, srcTweet, nil, false)
}

// tweets sends a tweet quoting or replying to the given tweets. srcTweet might
// be nil.
// Otherwise it just posts the message.
func (b *Bot) tweet(msg string, inReplyTo, srcTweet *twitter.Tweet, quote bool) {
	tweetMsg := ""
	params := &twitter.StatusUpdateParams{}
	sameTweets := false

	if inReplyTo == nil {
		tweetMsg = msg
		goto TWEET
	}

	sameTweets = srcTweet == nil || inReplyTo.ID == srcTweet.ID
	params.InReplyToStatusID = inReplyTo.ID

	switch {
	case sameTweets && !quote:
		// @user msg (reply thread)
		tweetMsg = fmt.Sprintf("@%s %s", inReplyTo.User.ScreenName, msg)
	case sameTweets && quote:
		// @user msg <permalink> (quote RT)
		tweetMsg = fmt.Sprintf(".@%s %s %s",
			inReplyTo.User.ScreenName,
			msg,
			permaLink(inReplyTo),
		)
	case !sameTweets && !quote:
		// @user @srcUser msg (reply thread)
		tweetMsg = fmt.Sprintf("@%s @%s %s",
			inReplyTo.User.ScreenName,
			srcTweet.User.ScreenName,
			msg,
		)
	case !sameTweets && quote:
		// @srcuser <replyPermalink> (quote RT mentioning src user)
		tweetMsg = fmt.Sprintf(".@%s %s %s",
			srcTweet.User.ScreenName,
			msg,
			permaLink(inReplyTo),
		)

	}

TWEET:
	log.Println("tweeting:", tweetMsg)
	newTweet, _, err := b.twClient.Statuses.Update(tweetMsg, params)
	if err != nil {
		log.Println(err)
		return
	}
	_ = newTweet
	// if quote { // then retweet my tweet after a minute
	// 	go func() {
	// 		time.Sleep(time.Minute)
	// 		_, _, err := b.twClient.Statuses.Retweet(newTweet.ID, nil)
	// 		log.Println("retweeted: ", tweetMsg)
	// 		if err != nil {
	// 			log.Println(err)
	// 			return
	// 		}
	// 	}()
	// }
	return
}

func permaLink(tweet *twitter.Tweet) string {
	return fmt.Sprintf("https://%s/%s/status/%s", twittercom, tweet.User.ScreenName, tweet.IDStr)
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
