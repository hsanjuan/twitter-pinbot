# twitter-pinbot

A Twitter bot to pin things on IPFS Cluster.

Run with a config file like:

```js
{
    "twitter_name": "@handle",
    "twitter_id":  "userid",
    "consumer_key":"<>",
    "consumer_secret":"<>",
    "access_key": "<>",
    "access_secret": "<>",
    "cluster_peer_addr": "<multiaddress to cluster peer>",
    "cluster_username": "<cluster basic auth username>",
    "cluster_password": "<cluster basic auth password>"
}
```

When [running the Docker container](https://hub.docker.com/r/hsanjuan/twitter-pinbot), mount `/data/config.json` 
