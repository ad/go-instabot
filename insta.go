package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/boltdb/bolt"
	"github.com/spf13/viper"
	"github.com/tducasse/goinsta"
	"github.com/tducasse/goinsta/response"
	"github.com/tducasse/goinsta/store"
)

// Insta is a goinsta.Instagram instance
var insta *goinsta.Instagram

var usersInfo = make(map[string]response.GetUsernameResponse)
var tagFeed = make(map[string]response.TagFeedsResponse)

// login will try to reload a previous session, and will create a new one if it can't
func login() {
	err := reloadSession()
	if err != nil {
		createAndSaveSession()
	}
}

func followFollowers(db *bolt.DB) {
	for msg := range followFollowersReq {
		time.Sleep(1 * time.Second)
		username := msg.CommandArguments()
		user, err := insta.GetUserByUsername(username)
		if err != nil {
			followFollowersRes <- TelegramResponse{fmt.Sprintf("%s", err)}
		} else {
			if user.User.IsPrivate {
				userFriendShip, err := insta.UserFriendShip(user.User.ID)
				check(err)
				if !userFriendShip.Following {
					followFollowersRes <- TelegramResponse{"User profile is private and we are not following, can't process"}
				}
			}

			followers, err := insta.TotalUserFollowers(user.User.ID)
			//log.Println(followers)
			check(err)
			var users = followers.Users
			if len(users) > 0 {
				rand.Seed(time.Now().UnixNano()) // do it once during app initialization
				Shuffle(users)
			}

			var limit = viper.GetInt("limits.maxSync")
			if limit <= 0 || limit >= 1000 {
				limit = 1000
			}

			today, _ := getStats(db, "refollow")
			if today > 0 {
				limit = limit - today
			}

			var allCount = int(math.Min(float64(len(users)), float64(limit)))
			if allCount == 0 && len(users) > 0 {
				followFollowersRes <- TelegramResponse{"Follow limit reached :("}
				fmt.Println("Follow limit reached :(")
			} else if allCount <= 0 {
				followFollowersRes <- TelegramResponse{"Followers not found :("}
				fmt.Println("Followers not found :(")
			} else {
				var current = 0

				fmt.Printf("\n%d followers!\n", allCount)
				followFollowersRes <- TelegramResponse{fmt.Sprintf("%d will be followed", allCount)}

				for _, user := range users {
					if current >= limit {
						continue
					}

					mutex.Lock()
					if state["refollow_cancel"] > 0 {
						state["refollow_cancel"] = 0
						mutex.Unlock()
						followFollowersRes <- TelegramResponse{"refollowwing canceled"}
						break
					}

					if user.IsPrivate {
						log.Printf("%s is private, skipping\n", user.Username)
					} else {
						previoslyFollowed, _ := getFollowed(db, user.Username)
						if previoslyFollowed != "" {
							log.Printf("%s previously followed at %s, skipping\n", user.Username, previoslyFollowed)
						} else {
							current++
							state["refollow"] = int(current * 100 / allCount)
							state["refollow_current"] = current
							state["refollow_all_count"] = allCount

							text := fmt.Sprintf("[%d/%d] refollowing %s (%d%%)\n", state["refollow_current"], state["refollow_all_count"], user.Username, state["refollow"])
							log.Println(text)
							followFollowersRes <- TelegramResponse{text}

							if !*dev {
								insta.Follow(user.ID)
								setFollowed(db, user.Username)
								incStats(db, "follow")
								incStats(db, "refollow")
								time.Sleep(10 * time.Second)
							} else {
								time.Sleep(1 * time.Second)
							}
						}
					}

					mutex.Unlock()
				}
				followFollowersRes <- TelegramResponse{fmt.Sprintf("\nRefollowed %d users!\n", current)}
			}
			mutex.Lock()
			state["refollow"] = -1
			editMessage["refollow"] = make(map[int]int)
			mutex.Unlock()
		}
	}
}

func syncFollowers(db *bolt.DB) {
	for msg := range unfollowReq {
		log.Println(msg)
		time.Sleep(1 * time.Second)
		following, err := insta.SelfTotalUserFollowing()
		check(err)
		followers, err := insta.SelfTotalUserFollowers()
		check(err)

		var users []response.User
		for _, user := range following.Users {
			if !contains(followers.Users, user) {
				users = append(users, user)
			}
		}

		var limit = viper.GetInt("limits.maxSync")
		if limit <= 0 || limit >= 1000 {
			limit = 1000
		}

		var daysBeforeUnfollow = viper.GetInt("limits.daysBeforeUnfollow")
		if daysBeforeUnfollow <= 1 || daysBeforeUnfollow >= 14 {
			daysBeforeUnfollow = 3
		}

		today, _ := getStats(db, "unfollow")
		if today > 0 {
			limit = limit - today
		}

		var allCount = int(math.Min(float64(len(users)), float64(limit)))
		if allCount > 0 {
			var current = 0

			fmt.Printf("%d users are not following you back!\n", allCount)
			unfollowRes <- TelegramResponse{fmt.Sprintf("%d will be unfollowed", allCount)}

			for _, user := range users {
				if current >= limit {
					continue
				}

				mutex.Lock()
				if state["unfollow_cancel"] > 0 {
					state["unfollow_cancel"] = 0
					mutex.Unlock()
					unfollowRes <- TelegramResponse{"Unfollowing canceled"}
					break
				}

				previoslyFollowed, _ := getFollowed(db, user.Username)
				if previoslyFollowed != "" {
					t, err := time.Parse("20060102", previoslyFollowed)

					if err != nil {
						fmt.Println(err)
					} else {
						duration := time.Since(t)
						if int(duration.Hours()) < (24 * daysBeforeUnfollow) {
							fmt.Printf("%s not followed us less then %f hours, skipping!\n", user.Username, duration.Hours())

							mutex.Unlock()
							continue
						}
					}
				}

				current++
				state["unfollow"] = int(current * 100 / allCount)
				state["unfollow_current"] = current
				state["unfollow_all_count"] = allCount

				mutex.Unlock()

				text := fmt.Sprintf("[%d/%d] Unfollowing %s (%d%%)\n", state["unfollow_current"], state["unfollow_all_count"], user.Username, state["unfollow"])
				log.Println(text)
				unfollowRes <- TelegramResponse{text}
				if !*dev {
					insta.UnFollow(user.ID)
					setFollowed(db, user.Username)
					incStats(db, "unfollow")
					time.Sleep(10 * time.Second)
				} else {
					time.Sleep(1 * time.Second)
				}
			}

			mutex.Lock()
			state["unfollow"] = -1
			editMessage["unfollow"] = make(map[int]int)
			mutex.Unlock()

			unfollowRes <- TelegramResponse{fmt.Sprintf("\nUnfollowed %d users are not following you back!\n", current)}
		}
	}
}

func getInput(text string) string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf(text)

	input, err := reader.ReadString('\n')
	check(err)
	return strings.TrimSpace(input)
}

// Checks if the user is in the slice
func contains(slice []response.User, user response.User) bool {
	for _, currentUser := range slice {
		if currentUser == user {
			return true
		}
	}
	return false
}

// Logins and saves the session
func createAndSaveSession() {
	insta = goinsta.New(instaUsername, instaPassword)
	err := insta.Login()
	check(err)

	key := createKey()
	bytes, err := store.Export(insta, key)
	check(err)
	err = ioutil.WriteFile("session", bytes, 0644)
	check(err)
	log.Println("Created and saved the session")
}

// reloadSession will attempt to recover a previous session
func reloadSession() error {
	if _, err := os.Stat("session"); os.IsNotExist(err) {
		return errors.New("No session found")
	}

	session, err := ioutil.ReadFile("session")
	check(err)
	log.Println("A session file exists")

	key, err := ioutil.ReadFile("key")
	check(err)

	insta, err = store.Import(session, key)
	if err != nil {
		return errors.New("Couldn't recover the session")
	}

	log.Println("Successfully logged in")
	return nil

}

// createKey creates a key and saves it to file
func createKey() []byte {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	check(err)
	err = ioutil.WriteFile("key", key, 0644)
	check(err)
	log.Println("Created and saved the key")
	return key
}

// Go through all the tags in the list
func loopTags(db *bolt.DB) {
	usersInfo = make(map[string]response.GetUsernameResponse)
	tagFeed = make(map[string]response.TagFeedsResponse)

	for msg := range followReq {
		log.Println(msg)
		time.Sleep(1 * time.Second)
		var allCount = len(tagsList)
		if allCount > 0 {
			var current = 0

			for tag := range tagsList {
				current++
				mutex.Lock()
				if state["follow_cancel"] > 0 {
					state["follow_cancel"] = 0
					state["follow"] = -1
					editMessage["follow"] = make(map[int]int)
					mutex.Unlock()
					followRes <- TelegramResponse{"Following canceled"}
					return
				}

				state["follow"] = int(current * 100 / allCount)
				state["follow_current"] = current
				state["follow_all_count"] = allCount
				mutex.Unlock()

				limitsConf := viper.GetStringMap("tags." + tag)

				// Some converting
				limits = map[string]int{
					"follow":  int(limitsConf["follow"].(float64)),
					"like":    int(limitsConf["like"].(float64)),
					"comment": int(limitsConf["comment"].(float64)),
				}
				// What we did so far
				numFollowed = 0
				numLiked = 0
				numCommented = 0

				text := fmt.Sprintf("\n[%d/%d] ➜ Current tag is %s (%d%%)\n", state["follow_current"], state["follow_all_count"], tag, state["follow"])
				log.Println(text)
				followRes <- TelegramResponse{text}
				browse(tag, db)
			}
			followRes <- TelegramResponse{"Finished"}
		}
		mutex.Lock()
		state["follow"] = -1
		editMessage["follow"] = make(map[int]int)
		mutex.Unlock()

		reportAsString := ""
		for tag, _ := range report {
			reportAsString += fmt.Sprintf("#%s: followed — %d, liked — %d, commented — %d\n", tag, report[tag]["follow"], report[tag]["like"], report[tag]["comment"])
		}

		// Displays the report on the screen / log file
		fmt.Println("\n\n\n" + reportAsString)

		// Sends the report to the email in the config file, if the option is enabled
		followRes <- TelegramResponse{reportAsString}
	}
}

// Browses the page for a certain tag, until we reach the limits
func browse(tag string, db *bolt.DB) {
	var i = 0
	for numFollowed < limits["follow"] || numLiked < limits["like"] || numCommented < limits["comment"] {
		// mutex.Lock()
		if state["follow_cancel"] > 0 {
			// mutex.Unlock()
			return
		}
		// mutex.Unlock()
		log.Println("Fetching the list of images for #" + tag)
		i++

		// Getting all the pictures we can on the first page
		// Instagram will return a 500 sometimes, so we will retry 10 times.
		// Check retry() for more info.

		var images, ok = tagFeed[tag]
		if ok {
			// log.Println("from cache #" + tag)
		} else {
			err := retry(10, 20*time.Second, func() (err error) {
				images, err = insta.TagFeed(tag)
				if err == nil {
					tagFeed[tag] = images
				}
				return
			})
			check(err)
		}

		goThrough(tag, db, images)

		if viper.IsSet("limits.maxRetry") && i > viper.GetInt("limits.maxRetry") {
			log.Println("Currently not enough images for this tag to achieve goals")
			break
		}
	}
}

// Goes through all the images for a certain tag
func goThrough(tag string, db *bolt.DB, images response.TagFeedsResponse) {
	var i = 1
	for _, image := range images.FeedsResponse.Items {
		// mutex.Lock()
		if state["follow_cancel"] > 0 {
			// mutex.Unlock()
			return
		}
		// mutex.Unlock()
		// Exiting the loop if there is nothing left to do
		if numFollowed >= limits["follow"] && numLiked >= limits["like"] && numCommented >= limits["comment"] {
			break
		}

		// Skip our own images
		if image.User.Username == instaUsername {
			continue
		}

		// Check if we should fetch new images for tag
		if i >= limits["follow"] && i >= limits["like"] && i >= limits["comment"] {
			break
		}

		// Getting the user info
		// Instagram will return a 500 sometimes, so we will retry 10 times.
		// Check retry() for more info.
		var posterInfo, ok = usersInfo[image.User.Username]
		if ok {
			// log.Println("from cache " + posterInfo.User.Username + " - for #" + tag)
		} else {
			err := retry(10, 20*time.Second, func() (err error) {
				posterInfo, err = insta.GetUserByID(image.User.ID)
				if err == nil {
					usersInfo[image.User.Username] = posterInfo
				}
				return
			})
			check(err)
		}

		poster := posterInfo.User
		followerCount := poster.FollowerCount

		//buildLine()

		// log.Println("Checking followers for " + poster.Username + " - for #" + tag)
		if followerCount < likeLowerLimit && followerCount < likeUpperLimit {
			log.Printf("%s has %d followers, less than min %d\n", poster.Username, followerCount, likeLowerLimit)
		} else if followerCount > likeUpperLimit {
			log.Printf("%s has %d followers, more than max %d\n", poster.Username, followerCount, likeUpperLimit)
		} else {
			log.Printf("%s has %d followers\n", poster.Username, followerCount)
		}
		i++

		// Will only follow and comment if we like the picture
		like := followerCount > likeLowerLimit && followerCount < likeUpperLimit && numLiked < limits["like"]
		follow := followerCount > followLowerLimit && followerCount < followUpperLimit && numFollowed < limits["follow"] && like
		comment := followerCount > commentLowerLimit && followerCount < commentUpperLimit && numCommented < limits["comment"] && like

		// Like, then comment/follow
		if like {
			if userLikesCount, ok := likesToAccountPerSession[posterInfo.User.Username]; ok {
				if userLikesCount < maxLikesToAccountPerSession {
					likeImage(tag, db, image, posterInfo)
				} else {
					log.Println("Likes count per user reached [" + poster.Username + "]")
				}
			} else {
				likeImage(tag, db, image, posterInfo)
			}

			previoslyFollowed, _ := getFollowed(db, posterInfo.User.Username)
			if previoslyFollowed != "" {
				log.Printf("%s already following (%s), skipping\n", posterInfo.User.Username, previoslyFollowed)
			} else {
				if comment {
					if !image.HasLiked {
						commentImage(tag, db, image)
					}
				}
				if follow {
					followUser(tag, db, posterInfo)
				}
			}

			// This is to avoid the temporary ban by Instagram
			time.Sleep(20 * time.Second)
		}
		// log.Printf("%s done\n\n", poster.Username)
	}
}

// Likes an image, if not liked already
func likeImage(tag string, db *bolt.DB, image response.MediaItemResponse, userInfo response.GetUsernameResponse) {
	log.Println("Liking the picture https://www.instagram.com/p/" + image.Code)
	if !image.HasLiked {
		if !*dev {
			insta.Like(image.ID)
		}
		// log.Println("Liked")
		numLiked++

		if _, ok := report[tag]; !ok {
			report[tag] = make(map[string]int)
		}
		report[tag]["like"]++
		incStats(db, "like")
		likesToAccountPerSession[userInfo.User.Username]++
	} else {
		// log.Println("Image already liked")
	}
}

// Comments an image
func commentImage(tag string, db *bolt.DB, image response.MediaItemResponse) {
	rand.Seed(time.Now().Unix())
	text := commentsList[rand.Intn(len(commentsList))]
	if !*dev {
		insta.Comment(image.ID, text)
	}
	log.Println("Commented " + text)
	numCommented++

	if _, ok := report[tag]; !ok {
		report[tag] = make(map[string]int)
	}
	report[tag]["comment"]++
	incStats(db, "comment")
}

// Follows a user, if not following already
func followUser(tag string, db *bolt.DB, userInfo response.GetUsernameResponse) {
	user := userInfo.User

	userFriendShip, err := insta.UserFriendShip(user.ID)
	check(err)
	// If not following already
	if !userFriendShip.Following {
		if !*dev {
			if user.IsPrivate {
				log.Printf("%s is private, skipping follow\n", user.Username)
			} else {
				log.Printf("Following %s\n", user.Username)
				insta.Follow(user.ID)
			}
		}
		// log.Println("Followed")
		numFollowed++
		if _, ok := report[tag]; !ok {
			report[tag] = make(map[string]int)
		}
		report[tag]["follow"]++
		incStats(db, "follow")
		setFollowed(db, user.Username)
	} else {
		log.Println("Already following " + user.Username)
	}
}
