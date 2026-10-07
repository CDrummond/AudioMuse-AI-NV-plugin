package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"audiomuse-navidrome-plugin/sonicsimilarity"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/types"
)

// Configuration keys (must match manifest.json)
const (
	configAPIUrl              = "apiUrl"
	configAPIToken            = "apiToken"
	configServer              = "server"
	configEliminateDuplicates = "eliminateDuplicates"
	configRadiusSimilarity    = "radiusSimilarity"
	configInstantMixSource    = "instantMixSource"
)

const (
	instantMixSimilarSong  = "similarSong"
	instantMixLyricsBySong = "lyricsBySong"
	instantMixHyperbolic   = "hyperbolic"
)

// Default values
const (
	defaultAPIUrl              = "http://192.168.3.203:8000"
	defaultArtistSimilarCount  = 10
	defaultEliminateDuplicates = true
	defaultRadiusSimilarity    = true
	defaultInstantMixSource    = instantMixSimilarSong
)

const (
	excludeSeedNever          = "never"
	excludeSeedFirst          = "first"
	excludeSeedAllIfFew       = "few"
	excludeSeedAll            = "all"
	christmasGenre            = "Christmas"
	filterCountFactor         = 5
	maxFilterTracks           = 300
	countToUseWhenAllFiltered = 2
	shuffleBlockSize          = 5
)

// Compile-time check that we implement necessary interfaces
var _ metadata.SimilarSongsByArtistProvider = (*audioMusePlugin)(nil)
var _ metadata.SimilarSongsByTrackProvider = (*audioMusePlugin)(nil)
var _ metadata.SimilarArtistsProvider = (*audioMusePlugin)(nil)
var _ sonicsimilarity.SonicSimilarity = (*audioMusePlugin)(nil)

// audioMuseTrackResponse represents a single track from AudioMuse-AI API
// and is used for both similar-track and path responses.
type audioMuseTrackResponse struct {
	ItemID     string  `json:"item_id"`
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	Album      string  `json:"album"`
	Distance   float64 `json:"distance"`
	Similarity float64 `json:"similarity"`
	IsSeed     bool    `json:"is_seed"`
}

type processOptions struct {
	FilteringActive bool
	ExcludeArtists  map[string]bool
	ExcludeAlbums   map[string]bool
	ExcludeSeed     string
	ExcludeExplict  bool
	MinDuration     int
	MaxDuration     int
	FilterXmas      bool
	Shuffle         bool
	Remove          bool
	SeedGenres      *map[string]bool
	GenresInGroups  *map[string]bool
}

func (t *audioMuseTrackResponse) UnmarshalJSON(data []byte) error {
	type track audioMuseTrackResponse
	decoded := track{Distance: -1, Similarity: -1}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = audioMuseTrackResponse(decoded)
	return nil
}

type audioMusePathResponse struct {
	Path []audioMuseTrackResponse `json:"path"`
}

type audioMuseResultsResponse struct {
	Results []audioMuseTrackResponse `json:"results"`
}

type audioMusePlugin struct{}

func init() {
	// NOTE: Navidrome re-instantiates the WASM module on every call, so init()
	// runs very frequently. Do not log here: it floods the Navidrome log.
	metadata.Register(&audioMusePlugin{})
	sonicsimilarity.Register(&audioMusePlugin{})
}

// getConfigString retrieves a string config value with a default fallback
func getConfigString(key, defaultValue string) string {
	if value, ok := pdk.GetConfig(key); ok && value != "" {
		return value
	}
	return defaultValue
}

// getConfigInt retrieves an integer config value with a default fallback
func getConfigInt(key string, defaultValue int) int {
	if value, ok := pdk.GetConfig(key); ok && value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// getConfigBool retrieves a boolean config value with a default fallback
func getConfigBool(key string, defaultValue bool) bool {
	if value, ok := pdk.GetConfig(key); ok && value != "" {
		return value == "true"
	}
	return defaultValue
}

// getConfigStringAsList retrieves a []string config value, returning an empty slice if unset
func getConfigStringAsList(key string) []string {
	str := getConfigString(key, "")
	if len(str) < 1 {
		return []string{}
	}

	return splitString(str, "\n")
}

// Convert a list of items into a 'set'
func listToSet(itemList []string) map[string]bool {
	itemSet := make(map[string]bool, len(itemList))
	for _, v := range itemList {
		itemSet[v] = true
	}
	return itemSet
}

// Check if ket is in values
func inSet(key string, values map[string]bool) bool {
	return key != "" && values[key]
}

// Get a navidrome track instance from its ID
func getTrackByID(songID string) *types.Track {
	matches, err := host.MatcherMatchSongs([]types.SongRef{
		{ID: songID},
	}, host.MatchOptions{})
	if err != nil || len(matches) == 0 || matches[0] == nil {
		return nil
	}
	return matches[0]
}

// Split sting on 'sep'
func splitString(str, sep string) []string {
	parts := strings.Split(str, sep)
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
	}
	return parts
}

// Get 'set' of genres from groups where seed genre is present
func getSeedGenres(track *types.Track) *map[string]bool {
	if track != nil {
		genreGroups := getConfigStringAsList("genreGroups")
		if len(genreGroups) > 0 {
			genres := []string{}

			for _, grp := range genreGroups {
				group := splitString(grp, ",")
				groupSet := listToSet(group)
				for _, genre := range track.Genres {
					if groupSet[genre] {
						genres = append(genres[:], group[:]...)
						break
					}
				}
			}
			genreSet := listToSet(genres)
			return &genreSet
		}
	}
	return nil
}

// Get 'set' of all genres that user has placed into genre groups
func getAllGenresInGroups() *map[string]bool {
	genreGroups := getConfigStringAsList("genreGroups")
	if len(genreGroups) > 0 {
		genres := []string{}
		for _, grp := range genreGroups {
			group := splitString(grp, ",")
			genres = append(genres[:], group[:]...)
		}
		genreSet := listToSet(genres)
		return &genreSet
	}
	return nil
}

// Initialise processOptions rules
func initProcessOptions(track *types.Track) processOptions {
	seedGenres := getSeedGenres(track)
	var genresInGroups *map[string]bool = nil
	if seedGenres == nil {
		genresInGroups = getAllGenresInGroups()
	}

	opts := processOptions{
		FilteringActive: false,
		ExcludeArtists:  listToSet(getConfigStringAsList("excludeArtists")),
		ExcludeAlbums:   listToSet(getConfigStringAsList("excludeAlbums")),
		ExcludeSeed:     getConfigString("excludeSeedArtist", excludeSeedNever),
		ExcludeExplict:  getConfigBool("excludeExplict", true),
		MinDuration:     getConfigInt("minDuration", 0),
		MaxDuration:     getConfigInt("maxDuration", 0),
		FilterXmas:      time.Now().Month() != 12 && getConfigBool("filterXmas", true),
		Shuffle:         getConfigBool("shuffle", true),
		Remove:          getConfigBool("remove", true),
		SeedGenres:      seedGenres,
		GenresInGroups:  genresInGroups,
	}
	opts.FilteringActive = len(opts.ExcludeArtists) > 0 || len(opts.ExcludeAlbums) > 0 ||
		opts.MinDuration > 0 || opts.MaxDuration > 0 ||
		opts.FilterXmas || opts.SeedGenres != nil || opts.GenresInGroups != nil || opts.ExcludeSeed != excludeSeedNever
	return opts
}

// Adjust count to take into account possibility of process options
func initReqCount(processOpts processOptions, count int) int {
	if processOpts.FilteringActive {
		count *= filterCountFactor
		if count > maxFilterTracks {
			count = maxFilterTracks
		}
	} else if processOpts.Remove || processOpts.Shuffle {
		count *= 2
	}
	return count
}

func logExclude(why string, track audioMuseTrackResponse) {
	pdk.Log(pdk.LogDebug, fmt.Sprintf("[AudioMuse] (EXCLUDE %s) %s by %s from %s", why, track.Title, track.Author, track.Album))
}

// Determine if a track should be filtered out of response
func filter(track audioMuseTrackResponse, processOpts processOptions) bool {
	if inSet(track.Author, processOpts.ExcludeArtists) {
		logExclude("Artist", track)
		return true
	}
	if inSet(track.Album, processOpts.ExcludeAlbums) {
		logExclude("Album", track)
		return true
	}
	if inSet(fmt.Sprintf("%s//%s", track.Author, track.Album), processOpts.ExcludeAlbums) {
		logExclude("Artist+Album", track)
		return true
	}
	if processOpts.MinDuration > 0 || processOpts.MaxDuration > 0 || processOpts.FilterXmas || len(processOpts.ExcludeAlbums) > 0 || nil != processOpts.GenresInGroups || nil != processOpts.SeedGenres {
		navTrack := getTrackByID(track.ItemID)
		if navTrack != nil {
			if processOpts.ExcludeExplict && len(navTrack.ExplicitStatus) > 0 {
				logExclude("Explicit", track)
				return true
			}
			if (processOpts.MinDuration > 0 && int(navTrack.Duration) < processOpts.MinDuration) || (processOpts.MaxDuration > 0 && int(navTrack.Duration) > processOpts.MaxDuration) {
				logExclude("Duration", track)
				return true
			}
			if len(processOpts.ExcludeAlbums) > 0 && inSet(fmt.Sprintf("%s//%s", navTrack.AlbumArtist, track.Album), processOpts.ExcludeAlbums) {
				logExclude("AlbumArtist+Album", track)
				return true
			}
			if processOpts.FilterXmas {
				for _, genre := range navTrack.Genres {
					if genre == christmasGenre {
						logExclude("Christmas", track)
						return true
					}
				}
			}
			if processOpts.SeedGenres != nil {
				// Seed genre is in a group, therefore candidate also needs to be in group
				for _, genre := range navTrack.Genres {
					if (*processOpts.SeedGenres)[genre] {
						// Match, so don't filter out
						return false
					}
				}
				logExclude("Seed Genres", track)
				return true
			} else if processOpts.GenresInGroups != nil {
				// Seed genre not in a group, but groups defined, therefore candidate also needs to NOT be in a group
				for _, genre := range navTrack.Genres {
					if (*processOpts.GenresInGroups)[genre] {
						// Matched so filter out
						logExclude("Genres", track)
						return true
					}
				}
			}
		}
	}
	return false
}

func shuffleRange(slice []audioMuseTrackResponse, start, end int) {
	// Ensure valid range
	if start < 0 || end > len(slice) || start >= end {
		return
	}

	// Calculate the number of elements to shuffle
	n := end - start

	// Shuffle the range [start, end)
	rand.Shuffle(n, func(i, j int) {
		slice[start+i], slice[start+j] = slice[start+j], slice[start+i]
	})
}

// process AudioMuse list via filtering, etc.
func process(seedTrack *types.Track, tracks []audioMuseTrackResponse, processOpts processOptions, count int) []audioMuseTrackResponse {
	if !processOpts.FilteringActive && !processOpts.Shuffle && !processOpts.Remove {
		return tracks
	}
	accepted := make([]audioMuseTrackResponse, 0, len(tracks))
	exSeedFirst := excludeSeedFirst == processOpts.ExcludeSeed
	exSeedAll := (excludeSeedAll == processOpts.ExcludeSeed) || (excludeSeedAllIfFew == processOpts.ExcludeSeed && count <= 8)

	used := 0
	useReq := count
	if processOpts.Shuffle {
		// If we are going to shuffle then we we also exclude consecutive artists - so might need more tracks to cater for this.
		useReq = count + (count / 2)
	}
	if processOpts.Remove && useReq < count*2 {
		useReq = count * 2
	}
	for idx, track := range tracks {
		if ((exSeedFirst && 0 == idx) || exSeedAll) && seedTrack != nil && (track.Author == (*seedTrack).Artist || track.Author == (*seedTrack).AlbumArtist) {
			logExclude(fmt.Sprintf("Seed Artist [%d]", idx), track)
			continue
		}
		if filter(track, processOpts) {
			continue
		}
		accepted = append(accepted, track)
		used++
		if used >= useReq {
			break
		}
	}

	// All filtered out??? Return first 2 - better than nothing?
	if used < 1 {
		for _, track := range tracks {
			accepted = append(accepted, track)
			used++
			if used >= countToUseWhenAllFiltered {
				break
			}
		}
	} else {
		blockSize := shuffleBlockSize
		if processOpts.Remove && processOpts.Shuffle && used >= shuffleBlockSize {
			blockSize += shuffleBlockSize / 2
		}
		if processOpts.Shuffle && used > 2 {
			if used <= blockSize {
				rand.Shuffle(used, func(i, j int) {
					accepted[i], accepted[j] = accepted[j], accepted[i]
				})
			} else {
				// Shuffle blocks of tracks
				for i := 0; i < used; i += blockSize {
					shuffleRange(accepted, i, i+blockSize)
				}
				if used%blockSize > 0 {
					shuffleRange(accepted, used-(blockSize-2), used)
				}
			}
		}

		if processOpts.Remove && blockSize > 4 && used >= blockSize {
			toRemove := shuffleBlockSize - blockSize
			if toRemove <= 0 || toRemove >= 4 {
				toRemove = 2
			}
			for i := 0; i < used-blockSize; i += blockSize {
				for j := range toRemove {
					index := (i * blockSize) + rand.IntN(blockSize-j)
					if index >= 0 && index < used {
						accepted = append(accepted[:index], accepted[index+1:]...)
						used = len(accepted)
					}
				}
			}
		}

		if used > 2 {
			// Now ensure don't have 2 tracks in a row from same artist
			filtered := make([]audioMuseTrackResponse, 0, len(accepted))
			lastArtist := ""
			filterCount := 0
			for _, track := range accepted {
				if track.Author == lastArtist {
					logExclude("Same Artist as prev", track)
					continue
				}
				lastArtist = track.Author
				filtered = append(filtered, track)
				filterCount += 1
				if filterCount >= count {
					break
				}
			}
			if filterCount >= 3 {
				return filtered
			}
		}
	}
	return accepted
}

// authHeaders returns a headers map with a Bearer token if configured, or nil otherwise.
func authHeaders() map[string]string {
	if token := getConfigString(configAPIToken, ""); token != "" {
		return map[string]string{
			"Authorization": "Bearer " + token,
		}
	}
	return nil
}

func jsonHeaders() map[string]string {
	headers := authHeaders()
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Type"] = "application/json"
	return headers
}

func (p *audioMusePlugin) GetSimilarSongsByTrack(input metadata.SimilarSongsByTrackRequest) (*metadata.SimilarSongsResponse, error) {
	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] GetSimilarSongsByTrack called for track ID: %s, Name: %s, Artist: %s", input.ID, input.Name, input.Artist))

	seedTrack := getTrackByID(input.ID)
	count := int(input.Count)
	processOpts := initProcessOptions(seedTrack)
	reqCount := initReqCount(processOpts, count)

	tracks, err := p.getAudioMuseSimilarTracks(input.ID, reqCount)
	if err != nil {
		return nil, err
	}

	processed := process(seedTrack, tracks, processOpts, count)

	// Convert to Navidrome SongRef format preserving order
	songs := make([]metadata.SongRef, 0, len(processed))
	for _, track := range processed {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("[AudioMuse] (INCLUDE) %s by %s from %s", track.Title, track.Author, track.Album))
		songs = append(songs, metadata.SongRef{
			ID:     track.ItemID,
			Name:   track.Title,
			Artist: track.Author,
			Album:  track.Album,
		})
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Returning %d songs to Navidrome", len(songs)))

	return &metadata.SimilarSongsResponse{Songs: songs}, nil
}

func (p *audioMusePlugin) getAudioMuseSimilarTracks(itemID string, count int) ([]audioMuseTrackResponse, error) {
	switch getConfigString(configInstantMixSource, defaultInstantMixSource) {
	case instantMixLyricsBySong:
		{
			tracks, err := getLyricsSimilarTracks(itemID, count)
			if err != nil {
				pdk.Log(pdk.LogDebug, fmt.Sprintf("[AudioMuse] %s failed, falling back to %s", configInstantMixSource, instantMixSimilarSong))
				return getSimilarTracks(itemID, count)
			} else {
				return tracks, err
			}
		}
	case instantMixHyperbolic:
		return getHyperbolicSimilarTracks(itemID, count)
	default:
		return getSimilarTracks(itemID, count)
	}
}

func getSimilarTracks(itemID string, count int) ([]audioMuseTrackResponse, error) {
	apiBaseURL := getConfigString(configAPIUrl, defaultAPIUrl)
	eliminateDuplicates := getConfigBool(configEliminateDuplicates, defaultEliminateDuplicates)
	radiusSimilarity := getConfigBool(configRadiusSimilarity, defaultRadiusSimilarity)

	params := url.Values{}
	params.Set("item_id", itemID)
	params.Set("n", strconv.Itoa(count))
	params.Set("eliminate_duplicates", strconv.FormatBool(eliminateDuplicates))
	params.Set("radius_similarity", strconv.FormatBool(radiusSimilarity))
	if server := getConfigString(configServer, ""); server != "" {
		params.Set("server", server)
	}

	apiURL := fmt.Sprintf("%s/api/similar_tracks?%s", apiBaseURL, params.Encode())
	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Calling similar_tracks API: %s", apiURL))

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:  "GET",
		URL:     apiURL,
		Headers: authHeaders(),
	})
	if err != nil {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: HTTP request failed: %v", err)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI HTTP request failed: %w", err)
	}

	if resp == nil {
		errMsg := "[AudioMuse] ERROR: empty HTTP response"
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI returned an empty HTTP response")
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] API response status: %d", resp.StatusCode))
	if resp.StatusCode != 200 {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: AudioMuse-AI returned status %d", resp.StatusCode)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI returned status %d", resp.StatusCode)
	}

	var tracks []audioMuseTrackResponse
	if err := json.Unmarshal(resp.Body, &tracks); err != nil {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: Failed to parse similar_tracks response: %v", err)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("failed to parse AudioMuse-AI similar tracks response: %w", err)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Successfully parsed %d similar tracks", len(tracks)))
	return tracks, nil
}

func getLyricsSimilarTracks(itemID string, count int) ([]audioMuseTrackResponse, error) {
	results, err := postAudioMuseSimilarTracks("/api/sem_grove/search", "sem_grove_search", itemID, count+1)
	if err != nil {
		return nil, err
	}

	tracks := make([]audioMuseTrackResponse, 0, len(results))
	for _, track := range results {
		if track.IsSeed {
			continue
		}
		if track.Similarity >= 0 {
			track.Distance = 1 - track.Similarity
		}
		tracks = append(tracks, track)
	}
	if count > 0 && len(tracks) > count {
		tracks = tracks[:count]
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Successfully parsed %d similar tracks", len(tracks)))
	return tracks, nil
}

func getHyperbolicSimilarTracks(itemID string, count int) ([]audioMuseTrackResponse, error) {
	results, err := postAudioMuseSimilarTracks("/api/hyperbolic/similar", "hyperbolic_similar", itemID, count)
	if err != nil {
		return nil, err
	}

	tracks := make([]audioMuseTrackResponse, 0, len(results))
	for _, track := range results {
		if track.Distance > 0 {
			track.Distance = track.Distance / (1 + track.Distance)
		}
		tracks = append(tracks, track)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Successfully parsed %d similar tracks", len(tracks)))
	return tracks, nil
}

func apiErrorMessage(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	if len(body) > 200 {
		return string(body[:200])
	}
	return string(body)
}

func postAudioMuseSimilarTracks(endpoint, label, itemID string, count int) ([]audioMuseTrackResponse, error) {
	apiBaseURL := getConfigString(configAPIUrl, defaultAPIUrl)

	body := map[string]any{"item_id": itemID, "limit": count}
	if server := getConfigString(configServer, ""); server != "" {
		body["server"] = server
	}

	payload, err := json.Marshal(body)
	if err != nil {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: Failed to encode %s request: %v", label, err)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("failed to encode AudioMuse-AI %s request: %w", label, err)
	}

	apiURL := apiBaseURL + endpoint
	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Calling %s API: %s", label, apiURL))

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:  "POST",
		URL:     apiURL,
		Headers: jsonHeaders(),
		Body:    payload,
	})
	if err != nil {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: HTTP request failed: %v", err)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI HTTP request failed: %w", err)
	}

	if resp == nil {
		errMsg := "[AudioMuse] ERROR: empty HTTP response"
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI returned an empty HTTP response")
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] API response status: %d", resp.StatusCode))
	if resp.StatusCode != 200 {
		apiError := apiErrorMessage(resp.Body)
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: AudioMuse-AI returned status %d: %s", resp.StatusCode, apiError)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("AudioMuse-AI returned status %d: %s", resp.StatusCode, apiError)
	}

	var result audioMuseResultsResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		errMsg := fmt.Sprintf("[AudioMuse] ERROR: Failed to parse %s response: %v", label, err)
		pdk.Log(pdk.LogError, errMsg)
		return nil, fmt.Errorf("failed to parse AudioMuse-AI %s response: %w", label, err)
	}

	return result.Results, nil
}

func (p *audioMusePlugin) GetSonicSimilarTracks(input sonicsimilarity.GetSonicSimilarTracksRequest) (sonicsimilarity.SonicSimilarityResponse, error) {
	if input.Song.ID == "" {
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("song.id is required")
	}

	count := int(input.Count)
	if count <= 0 {
		count = 10
	}

	seedTrack := getTrackByID(input.Song.ID)
	processOpts := initProcessOptions(seedTrack)
	reqCount := initReqCount(processOpts, count)
	tracks, err := p.getAudioMuseSimilarTracks(input.Song.ID, reqCount)
	if err != nil {
		return sonicsimilarity.SonicSimilarityResponse{}, err
	}

	processed := process(seedTrack, tracks, processOpts, count)
	matches := make([]sonicsimilarity.SonicMatch, 0, len(processed))
	for _, track := range processed {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("[AudioMuse] (INCLUDE) %s by %s from %s", track.Title, track.Author, track.Album))
		matches = append(matches, sonicsimilarity.SonicMatch{
			Song: metadata.SongRef{
				ID:     track.ItemID,
				Name:   track.Title,
				Artist: track.Author,
				Album:  track.Album,
			},
			Similarity: normalizeSimilarity(track.Distance),
		})
	}

	return sonicsimilarity.SonicSimilarityResponse{Matches: matches}, nil
}

func (p *audioMusePlugin) FindSonicPath(input sonicsimilarity.FindSonicPathRequest) (sonicsimilarity.SonicSimilarityResponse, error) {
	if input.StartSong.ID == "" || input.EndSong.ID == "" {
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("startSong.id and endSong.id are required")
	}

	count := int(input.Count)
	if count <= 0 {
		count = 25
	}

	apiBaseURL := getConfigString(configAPIUrl, defaultAPIUrl)
	params := url.Values{}
	params.Set("start_song_id", input.StartSong.ID)
	params.Set("end_song_id", input.EndSong.ID)
	params.Set("count", strconv.Itoa(count))
	params.Set("max_steps", strconv.Itoa(count))
	params.Set("path_fix_size", "false")
	params.Set("mood_pct", "100")
	if server := getConfigString(configServer, ""); server != "" {
		params.Set("server", server)
	}

	apiURL := fmt.Sprintf("%s/api/find_path?%s", apiBaseURL, params.Encode())
	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Calling FindSonicPath API from %s to %s: %s", input.StartSong.ID, input.EndSong.ID, apiURL))

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:  "GET",
		URL:     apiURL,
		Headers: authHeaders(),
	})
	if err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("[AudioMuse] ERROR: HTTP request failed: %v", err))
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("AudioMuse-AI HTTP request failed: %w", err)
	}

	if resp == nil {
		pdk.Log(pdk.LogError, "[AudioMuse] ERROR: empty HTTP response")
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("AudioMuse-AI returned an empty HTTP response")
	}

	if resp.StatusCode != 200 {
		pdk.Log(pdk.LogError, fmt.Sprintf("[AudioMuse] ERROR: AudioMuse-AI returned status %d", resp.StatusCode))
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("AudioMuse-AI returned status %d", resp.StatusCode)
	}

	var result audioMusePathResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		pdk.Log(pdk.LogError, fmt.Sprintf("[AudioMuse] ERROR: Failed to parse FindSonicPath response: %v", err))
		return sonicsimilarity.SonicSimilarityResponse{}, fmt.Errorf("failed to parse AudioMuse-AI find path response: %w", err)
	}

	matches := make([]sonicsimilarity.SonicMatch, 0, len(result.Path))
	for _, item := range result.Path {
		matches = append(matches, sonicsimilarity.SonicMatch{
			Song: metadata.SongRef{
				ID:     item.ItemID,
				Name:   item.Title,
				Artist: item.Author,
				Album:  item.Album,
			},
			Similarity: -1.0,
		})
	}

	return sonicsimilarity.SonicSimilarityResponse{Matches: matches}, nil
}

func normalizeSimilarity(distance float64) float64 {
	if distance < 0 {
		return -1
	}
	similarity := 1.0 - distance
	if similarity < 0 {
		similarity = 0
	}
	if similarity > 1 {
		similarity = 1
	}
	return similarity
}

func (p *audioMusePlugin) GetSimilarSongsByArtist(input metadata.SimilarSongsByArtistRequest) (*metadata.SimilarSongsResponse, error) {
	artists, err := getSimilarArtists(input.ID, true)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)

	// songSlices contains artist songs in alternating order: [baseArtist, relatedArtist1, baseArtist, relatedArtist2, ...]
	songSlices := [][]metadata.SongRef{}

	for _, a := range artists {
		var artist1Songs, artist2Songs []metadata.SongRef

		for _, cm := range a.ComponentMatches {
			for _, s := range cm.Artist1RepresentativeSongs {

				if s.ItemID == "" {
					continue
				}
				if seen[s.ItemID] {
					continue
				}

				seen[s.ItemID] = true
				artist1Songs = append(artist1Songs, metadata.SongRef{ID: s.ItemID, Name: s.Title})
			}

			for _, s := range cm.Artist2RepresentativeSongs {
				if s.ItemID == "" {
					continue
				}

				if seen[s.ItemID] {
					continue
				}

				seen[s.ItemID] = true
				artist2Songs = append(artist2Songs, metadata.SongRef{ID: s.ItemID, Name: s.Title})
			}
		}

		if len(artist1Songs) > 0 {
			songSlices = append(songSlices, artist1Songs)
		}
		if len(artist2Songs) > 0 {
			songSlices = append(songSlices, artist2Songs)
		}
	}

	songs := make([]metadata.SongRef, 0, input.Count)

	// get songs from our slices until we have enough or we ran out
	artistID := 0
	for len(songs) < int(input.Count) && len(songSlices) > 0 {
		song := songSlices[artistID][0] // take a song
		songs = append(songs, song)

		songSlices[artistID] = songSlices[artistID][1:] // remove it from the pool

		if len(songSlices[artistID]) == 0 {
			// this slice has no more songs, remove it
			songSlices = slices.Delete(songSlices, artistID, artistID+1)
			if len(songSlices) == 0 {
				break
			}
		} else {
			// else, go to the next slice
			artistID++
		}

		artistID = artistID % len(songSlices) // loop around if needed
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Returning %d artist-related songs to Navidrome", len(songs)))

	return &metadata.SimilarSongsResponse{Songs: songs}, nil
}

// GetSimilarArtists implements metadata.SimilarArtistsProvider.
func (p *audioMusePlugin) GetSimilarArtists(input metadata.SimilarArtistsRequest) (*metadata.SimilarArtistsResponse, error) {
	artists, err := getSimilarArtists(input.ID, false)
	if err != nil {
		return nil, err
	}

	res := &metadata.SimilarArtistsResponse{
		Artists: make([]metadata.ArtistRef, 0, len(artists)),
	}

	seen := make(map[string]bool)
	for _, a := range artists {
		if a.ArtistID == "" {
			continue
		}
		if a.ArtistID == input.ID {
			continue
		}
		if seen[a.ArtistID] {
			continue
		}
		seen[a.ArtistID] = true

		res.Artists = append(res.Artists, metadata.ArtistRef{
			ID:   a.ArtistID,
			Name: a.Artist,
		})
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("[AudioMuse] Returning %d related artists to Navidrome", len(res.Artists)))

	return res, nil
}

func main() {}
