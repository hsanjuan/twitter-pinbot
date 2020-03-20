# twitter-pinbot

A Twitter bot to pin things on [IPFS Cluster](https://cluster.ipfs.io).

Assuming you have a working IPFS Cluster, you can run with a config file like:

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

When [running the Docker container](https://hub.docker.com/r/hsanjuan/twitter-pinbot), mount the configuration into `/data/config.json` 

## Tutorial

A full tutorial covering how to set up all the pieces is available here:

https://simpleaswater.com/ipfs-cluster-twitter-pinbot/
