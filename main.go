package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	cid "github.com/ipfs/go-cid"
	clusterapi "github.com/ipfs/ipfs-cluster/api"
	"github.com/ipfs/ipfs-cluster/api/rest/client"
	multiaddr "github.com/multiformats/go-multiaddr"
)

// ConfigFile is the path of the default configuration file
var ConfigFile = "config.json"

const (
	noAction action = iota
	pin
	unpin
)

type action int

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
	clusterClient *client.Client

	follows sync.Map
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
	clusterClient, err := client.NewClient(&client.Config{
		PeerAddr: peerAddr,
		Username: cfg.ClusterUsername,
		Password: cfg.ClusterPassword,
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
	}

	//bot.fetchFollowing()
	//go bot.watchFollowing()
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
	following, _, err := b.twClient.Friends.List(
		&twitter.FriendListParams{})
	if err != nil {
		log.Fatal(err)
	}
	for _, u := range following.Users {
		b.follows.Store(u.ID, struct{}{})
	}
}

func (b *Bot) watchFollowing() {
	for {
		select {
		case <-b.ctx.Done():
		default:
			b.fetchFollowing()
		}
		time.Sleep(30 * time.Second)
	}
}

func (b *Bot) parseTweet(msg string) (action, *cid.Cid, string, error) {
	lines := strings.Split(msg, "\n")
	if len(lines) < 1 {
		return noAction, nil, "", nil
	}

	words := strings.Split(strings.Replace(lines[0], ":", " ", -1), " ")

	if len(words) < 3 {
		return noAction, nil, "", nil
	}

	if words[0] != b.name {
		return noAction, nil, "", nil
	}

	action := noAction

	switch words[1] {
	case "pin":
		action = pin
	case "unpin":
		return noAction, nil, "", nil // let's not support it yet
		// action = unpin
	}

	c, err := cid.Decode(words[2])
	if err != nil {
		return noAction, nil, "", err
	}

	name := ""
	if len(words) >= 3 {
		name = strings.Join(words[3:], " ")
	}

	return action, c, name, nil
}

func (b *Bot) watchTweets() {
	log.Println("watching tweets")

	params := &twitter.StreamFilterParams{
		Follow:        []string{b.id},
		StallWarnings: twitter.Bool(true),
	}

	stream, err := b.twClient.Streams.Filter(params)
	if err != nil {
		log.Fatal(err)
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
	log.Printf("%+v\n", tweet.Text)
	//log.Printf("%+v\n", tweet.User)
	action, c, name, err := b.parseTweet(tweet.Text)
	//log.Println(action, c, name, err)
	if err != nil {
		b.tweet(err.Error(), tweet)
	}

	if action == noAction {
		return
	}

	var target clusterapi.TrackerStatus = clusterapi.TrackerStatusPinned

	switch action {
	case pin:
		log.Println("pin", c)
		b.clusterClient.Pin(c, 0, 0, name)
	case unpin:
		log.Println("unpin", c)
		b.clusterClient.Unpin(c)
		target = clusterapi.TrackerStatusUnpinned
	}

	_, err = b.clusterClient.WaitFor(b.ctx, client.StatusFilterParams{
		Cid:       c,
		Local:     false,
		Target:    target,
		CheckFreq: 15 * time.Second,
	})
	if err != nil {
		b.tweet(err.Error(), tweet)
	} else {
		b.tweet(fmt.Sprintf("%s %s", target, c), tweet)
	}
}

func (b *Bot) tweet(msg string, origTweet *twitter.Tweet) {
	var err error
	if origTweet != nil {
		_, _, err = b.twClient.Statuses.Update(
			fmt.Sprintf("%s", msg),
			&twitter.StatusUpdateParams{
				InReplyToStatusID: origTweet.ID},
		)
		if err != nil {
			log.Println(err)
		}
		return
	}

	_, _, err = b.twClient.Statuses.Update(msg, nil)
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
	log.Println(<-ch)
	bot.Kill()
}
