package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/kylods/kFeed/internal/database"
	_ "github.com/lib/pq"
)

// Definition for handlers that require authentication
type authedHandler func(http.ResponseWriter, *http.Request, database.User)

// For accessing the DB server, used in main()
type apiConfig struct {
	DB *database.Queries
}

// dateLayouts is a slice of potential date layouts RSS feeds might use
var dateLayouts = []string{
	time.RFC1123,
	time.RFC1123Z,
	"Mon, 02 Jan 2006 15:04:05 MST",
	// Add more layouts as needed
}

// Used in databaseFeedToFeed()
type Feed struct {
	ID            uuid.UUID  `json:"id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Name          string     `json:"name"`
	Url           string     `json:"url"`
	UserID        uuid.UUID  `json:"user_id"`
	LastFetchedAt *time.Time `json:"last_fetched_at"`
}

// Used in databasePostToPost()
type Post struct {
	ID          uuid.UUID `json:"id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Title       string    `json:"title"`
	Url         string    `json:"url"`
	Description string    `json:"description"`
	PublishedAt time.Time `json:"published_at"`
	FeedID      uuid.UUID `json:"feed_id"`
}

// Structs for RSS Feed data
type Rss struct {
	Channel Channel `xml:"channel"`
}

type Channel struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Items       []Item `xml:"item"`
}

type Item struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
}

func main() {
	// Load env variables from ".env" & opens a connection to the PostgreSQL server
	godotenv.Load()
	port := os.Getenv("PORT")
	dbURL := os.Getenv("DBURL")
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatal(fmt.Printf("Could not open connection to database: %v", err))
	}

	// Initialize apiCfg, so handler functions can access the DB connection.
	dbQueries := database.New(db)
	apiCfg := apiConfig{}
	apiCfg.DB = dbQueries

	// Routers & endpoints
	v1Router := chi.NewRouter()
	v1Router.Post("/users", apiCfg.handlerUsersPost)
	v1Router.Get("/users", apiCfg.middlewareAuth(apiCfg.handlerUsersGet))
	v1Router.Post("/feeds", apiCfg.middlewareAuth(apiCfg.handlerFeedsPost))
	v1Router.Get("/feeds", apiCfg.handlerFeedsGet)
	v1Router.Post("/feed_follows", apiCfg.middlewareAuth(apiCfg.handlerFeedFollowsPost))
	v1Router.Delete("/feed_follows/{id}", apiCfg.middlewareAuth(apiCfg.handlerFeedFollowsDelete))
	v1Router.Get("/feed_follows", apiCfg.middlewareAuth(apiCfg.handlerFeedFollowsGet))
	v1Router.Get("/posts", apiCfg.middlewareAuth(apiCfg.handlerPostsGet))
	v1Router.Get("/readiness", handlerReadinessGet)
	v1Router.Get("/err", errTest)

	mainRouter := chi.NewRouter()
	mainRouter.Use(cors.Handler(cors.Options{}))
	mainRouter.Mount("/v1", v1Router)

	// Start the worker for fetching feeds
	go apiCfg.fetchFeedsWorker()

	// Initialize server & starts listening for connections
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mainRouter,
	}

	log.Fatal(srv.ListenAndServe())
}

// Creates a user in the DB
func (cfg *apiConfig) handlerUsersPost(w http.ResponseWriter, r *http.Request) {
	type parameters struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(r.Body)
	params := parameters{}
	err := decoder.Decode(&params)
	if err != nil {
		// an error will be thrown if the JSON is invalid or has the wrong types
		// any missing fields will have their values set to their zero value
		log.Printf("Error decoding parameters: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	if params.Name == "" {
		respondWithError(w, 400, "Name cannot be empty")
		return
	}

	userParams := database.CreateUserParams{
		ID:        uuid.New(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Name:      params.Name,
	}
	user, err := cfg.DB.CreateUser(r.Context(), userParams)
	if err != nil {
		log.Printf("Error creating user: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	respondWithJSON(w, 201, user)
}

// Retrieves authenticated user
func (cfg *apiConfig) handlerUsersGet(w http.ResponseWriter, r *http.Request, user database.User) {
	respondWithJSON(w, 200, user)
}

// Create a feed in the DB
func (cfg *apiConfig) handlerFeedsPost(w http.ResponseWriter, r *http.Request, user database.User) {
	type parameters struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	decoder := json.NewDecoder(r.Body)
	params := parameters{}
	err := decoder.Decode(&params)
	if err != nil {
		// an error will be thrown if the JSON is invalid or has the wrong types
		// any missing fields will have their values in the struct set to their zero value
		log.Printf("Error decoding parameters: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	if params.Name == "" || params.URL == "" {
		respondWithError(w, 400, "Fields cannot be empty")
		return
	}

	feedParams := database.CreateFeedParams{
		ID:        uuid.New(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Name:      params.Name,
		Url:       params.URL,
		UserID:    user.ID,
	}
	dbFeed, err := cfg.DB.CreateFeed(r.Context(), feedParams)
	if err != nil {
		log.Printf("Error creating feed: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	feed := databaseFeedToFeed(dbFeed)

	followParams := database.FollowFeedParams{
		ID:        uuid.New(),
		UserID:    user.ID,
		FeedID:    feed.ID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	feedFollow, err := cfg.DB.FollowFeed(r.Context(), followParams)
	if err != nil {
		log.Printf("Error following feed: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}

	payload := struct {
		Feed       Feed                `json:"feed"`
		FeedFollow database.FeedFollow `json:"feed_follow"`
	}{
		Feed:       feed,
		FeedFollow: feedFollow,
	}
	respondWithJSON(w, 201, payload)
}

// Retrieves all feeds
func (cfg *apiConfig) handlerFeedsGet(w http.ResponseWriter, r *http.Request) {
	feeds, err := cfg.DB.GetAllFeeds(r.Context())
	if err != nil {
		respondWithError(w, 500, "Internal Server Error")
		return
	}
	respondWithJSON(w, 200, feeds)
}

// Follows a feed
func (cfg *apiConfig) handlerFeedFollowsPost(w http.ResponseWriter, r *http.Request, user database.User) {
	type parameters struct {
		FeedID string `json:"feed_id"`
	}
	decoder := json.NewDecoder(r.Body)
	params := parameters{}
	err := decoder.Decode(&params)
	if err != nil {
		// an error will be thrown if the JSON is invalid or has the wrong types
		// any missing fields will have their values in the struct set to their zero value
		log.Printf("Error decoding parameters: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}

	feedID, err := uuid.Parse(params.FeedID)
	if err != nil {
		respondWithError(w, 400, "Invalid FeedID")
	}
	followParams := database.FollowFeedParams{
		ID:        uuid.New(),
		UserID:    user.ID,
		FeedID:    feedID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	follow, err := cfg.DB.FollowFeed(r.Context(), followParams)
	if err != nil {
		log.Printf("Error following feed: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	respondWithJSON(w, 201, follow)
}

// Unfollows a feed
func (cfg *apiConfig) handlerFeedFollowsDelete(w http.ResponseWriter, r *http.Request, user database.User) {
	feedFollowString := chi.URLParam(r, "id")
	feedFollowUUID, err := uuid.Parse(feedFollowString)
	if err != nil {
		log.Printf("Error parsing id: %s", err)
		respondWithError(w, 400, "Invalid FollowFeedID")
		return
	}

	unfollowParams := database.UnfollowFeedParams{
		ID:     feedFollowUUID,
		UserID: user.ID,
	}
	err = cfg.DB.UnfollowFeed(r.Context(), unfollowParams)
	if err != nil {
		log.Printf("Error unfollowing: %s", err)
		respondWithError(w, 500, "Internal server error")
		return
	}
	respondWithJSON(w, 200, "OK")
}

// Gets all followed feeds
func (cfg *apiConfig) handlerFeedFollowsGet(w http.ResponseWriter, r *http.Request, user database.User) {
	feedFollows, err := cfg.DB.GetFollowedFeeds(r.Context(), user.ID)
	if err != nil {
		respondWithError(w, 500, "Internal server error")
		return
	}
	respondWithJSON(w, 200, feedFollows)
}

// Gets all posts for user's followed feeds
func (cfg *apiConfig) handlerPostsGet(w http.ResponseWriter, r *http.Request, user database.User) {
	limitStr := r.URL.Query().Get("limit")
	limitInt := 20
	if limitStr != "" {
		if i, err := strconv.Atoi(limitStr); err == nil {
			limitInt = i
		}
	}
	getPostsParams := database.GetPostsByUserParams{
		UserID: user.ID,
		Limit:  int32(limitInt),
	}

	posts, err := cfg.DB.GetPostsByUser(r.Context(), getPostsParams)
	if err != nil {
		respondWithError(w, 500, "Internal server error")
		return
	}

	var payload []Post

	for _, post := range posts {
		payload = append(payload, databasePostToPost(post))
	}
	respondWithJSON(w, 200, payload)
}

// Returns 200 status
func handlerReadinessGet(w http.ResponseWriter, r *http.Request) {
	response := struct {
		Status string `json:"status"`
	}{
		Status: "ok",
	}
	respondWithJSON(w, 200, response)
}

// Returns 500 status
func errTest(w http.ResponseWriter, r *http.Request) {
	respondWithError(w, 500, "Internal Server Error")
}

// Helper func that converts payload to JSON and responds to an HTTP request
func respondWithJSON(w http.ResponseWriter, status int, payload interface{}) {
	dat, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error encoding parameters: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(dat)
}

// Helper func that responds to an HTTP request
func respondWithError(w http.ResponseWriter, code int, msg string) {
	response := struct {
		Error string `json:"error"`
	}{
		Error: msg,
	}
	respondWithJSON(w, code, response)
}

// Helper func that converts database.Feed to Feed, for better looking JSON responses
func databaseFeedToFeed(dbFeed database.Feed) Feed {
	feed := Feed{
		ID:        dbFeed.ID,
		CreatedAt: dbFeed.CreatedAt,
		UpdatedAt: dbFeed.UpdatedAt,
		Name:      dbFeed.Name,
		Url:       dbFeed.Url,
		UserID:    dbFeed.UserID,
	}
	// If dbFeed.LastFetchedAt is NULL, keep the zero value (nil) of feed.LastFetchedAt
	if dbFeed.LastFetchedAt.Valid {
		feed.LastFetchedAt = &dbFeed.LastFetchedAt.Time
	}
	return feed
}

// Helper func that converts database.Post to Post, for better looking JSON responses
func databasePostToPost(dbPost database.Post) Post {
	post := Post{
		ID:        dbPost.ID,
		CreatedAt: dbPost.CreatedAt,
		UpdatedAt: dbPost.UpdatedAt,
		Title:     dbPost.Title,
		Url:       dbPost.Url,
		FeedID:    dbPost.FeedID,
	}
	// If NULL, keep the zero value (nil)
	if dbPost.Description.Valid {
		post.Description = dbPost.Description.String
	}
	if dbPost.PublishedAt.Valid {
		post.PublishedAt = dbPost.PublishedAt.Time
	}
	return post
}

// Middleware helper that authenticates a user before handing off the request to the handler
func (cfg *apiConfig) middlewareAuth(handler authedHandler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("Authorization")
		apiKey = strings.TrimPrefix(apiKey, "ApiKey ")

		user, err := cfg.DB.GetUserByAPIKey(r.Context(), apiKey)
		if err != nil {
			respondWithError(w, 400, "Unauthorized")
			return
		}
		handler(w, r, user)
	})
}

// Background goroutine for updating feeds
func (cfg *apiConfig) fetchFeedsWorker() {
	// Initialize variables & helper function
	ctx := context.TODO()
	ticker := time.Tick(time.Minute)
	fetchAndMarkDone := func(wg *sync.WaitGroup, feed database.Feed) {
		// Fetch each feed's data
		defer wg.Done()
		rss, err := fetchRSSFeedData(feed.Url)
		cfg.DB.MarkFeedFetched(ctx, feed.ID)
		if err != nil {
			fmt.Printf("Error fetching %v: %v\n", feed.Url, err)
			return
		}

		fmt.Printf("Fetched %v with %v posts!\n", rss.Channel.Title, len(rss.Channel.Items))

		// Recursively adds each post to the database
		for _, post := range rss.Channel.Items {
			// Attempts to parse posts 'description' & 'published date' to sql.NullString & sql.NullTime types respectively
			var postDescription sql.NullString
			var postPubDate sql.NullTime
			if post.Description != "" {
				postDescription.String = post.Description
				postDescription.Valid = true
			}
			if post.PubDate != "" {
				t, err := parseDate(post.PubDate)
				if err == nil {
					postPubDate.Valid = true
					postPubDate.Time = t
				}
			}

			// Assembles post data into a struct, then passes it to the database
			postParams := database.AddPostParams{
				ID:          uuid.New(),
				Title:       post.Title,
				Url:         post.Link,
				Description: postDescription,
				PublishedAt: postPubDate,
				FeedID:      feed.ID,
			}
			cfg.DB.AddPost(ctx, postParams)
		}
	}
	for {
		// Only lets the loop run once every minute, or the duration set on "ticker"s initialization
		<-ticker

		feedsToFetch, err := cfg.DB.GetNextFeedsToFetch(ctx, 10)
		if err != nil {
			fmt.Printf("Error fetching feeds: %v", err)
			continue
		}

		fmt.Printf("Fetching %v feeds...\n", len(feedsToFetch))

		// Creates a goroutine for each feed to fetch
		waitGroup := sync.WaitGroup{}
		waitGroup.Add(len(feedsToFetch))
		for _, feed := range feedsToFetch {
			go fetchAndMarkDone(&waitGroup, feed)
		}
		// Waits until all goroutines have finished
		waitGroup.Wait()
		fmt.Println("Finished processing feeds!")
	}
}

// Fetches data from an RSS feed
func fetchRSSFeedData(url string) (Rss, error) {
	resp, err := http.Get(url)
	if err != nil {
		return Rss{}, fmt.Errorf("GET error: %v", err)
	}
	defer resp.Body.Close()

	// Checks status code & content-type header
	if resp.StatusCode != http.StatusOK {
		return Rss{}, fmt.Errorf("status error: %v", resp.StatusCode)
	}
	if contentType := resp.Header.Get("content-type"); contentType != "application/xml" {
		return Rss{}, fmt.Errorf("invalid response 'content-type': %v", contentType)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Rss{}, fmt.Errorf("read body: %v", err)
	}

	rssFeed := Rss{}
	err = xml.Unmarshal(data, &rssFeed)
	if err != nil {
		return Rss{}, fmt.Errorf("XML decode error: %v", err)
	}

	return rssFeed, nil
}

func parseDate(dateStr string) (time.Time, error) {
	var t time.Time
	var err error
	for _, layout := range dateLayouts {
		t, err = time.Parse(layout, dateStr)
		if err == nil {
			return t, nil
		}
	}
	return t, err
}
