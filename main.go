package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
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
	v1Router.Get("/readiness", handlerReadinessGet)
	v1Router.Get("/err", errTest)

	mainRouter := chi.NewRouter()
	mainRouter.Use(cors.Handler(cors.Options{}))
	mainRouter.Mount("/v1", v1Router)

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
	feed, err := cfg.DB.CreateFeed(r.Context(), feedParams)
	if err != nil {
		log.Printf("Error creating feed: %s", err)
		respondWithError(w, 500, "Something went wrong")
		return
	}
	respondWithJSON(w, 201, feed)
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
