package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"plandex-server/db"
	"sync"

	"github.com/gorilla/mux"
	"github.com/plandex/plandex/shared"
)

func ListContextHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for ListContextHandler")

	auth := authenticate(w, r, true)
	if auth == nil {
		return
	}

	vars := mux.Vars(r)
	planId := vars["planId"]
	log.Println("planId: ", planId)

	if authorizePlan(w, planId, auth) == nil {
		return
	}

	var err error
	unlockFn := lockRepo(w, r, auth, db.LockScopeRead)
	if unlockFn == nil {
		return
	} else {
		defer (*unlockFn)(err)
	}

	dbContexts, err := db.GetPlanContexts(auth.OrgId, planId, false)

	if err != nil {
		log.Printf("Error getting contexts: %v\n", err)
		http.Error(w, "Error getting contexts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var apiContexts []*shared.Context

	for _, dbContext := range dbContexts {
		apiContexts = append(apiContexts, dbContext.ToApi())
	}

	bytes, err := json.Marshal(apiContexts)

	if err != nil {
		log.Printf("Error marshalling contexts: %v\n", err)
		http.Error(w, "Error marshalling contexts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(bytes)
}

func LoadContextHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for LoadContextHandler")

	auth := authenticate(w, r, true)
	if auth == nil {
		return
	}

	vars := mux.Vars(r)
	planId := vars["planId"]
	branchName := vars["branch"]
	log.Println("planId: ", planId)

	plan := authorizePlan(w, planId, auth)
	if plan == nil {
		return
	}

	// read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var requestBody shared.LoadContextRequest
	if err := json.Unmarshal(body, &requestBody); err != nil {
		log.Printf("Error parsing request body: %v\n", err)
		http.Error(w, "Error parsing request body", http.StatusBadRequest)
		return
	}

	res, _ := loadContexts(w, r, auth, &requestBody, planId, branchName)
	if res == nil {
		return
	}

	bytes, err := json.Marshal(res)

	if err != nil {
		log.Printf("Error marshalling response: %v\n", err)
		http.Error(w, "Error marshalling response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("Successfully processed LoadContextHandler request")

	w.Write(bytes)
}

func UpdateContextHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for UpdateContextHandler")

	auth := authenticate(w, r, true)
	if auth == nil {
		return
	}

	vars := mux.Vars(r)
	planId := vars["planId"]
	branchName := vars["branch"]
	log.Println("planId: ", planId)

	plan := authorizePlan(w, planId, auth)
	if plan == nil {
		return
	}

	branch, err := db.GetDbBranch(planId, branchName)
	if err != nil {
		log.Printf("Error getting branch: %v\n", err)
		http.Error(w, "Error getting branch: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var requestBody shared.UpdateContextRequest
	if err := json.Unmarshal(body, &requestBody); err != nil {
		log.Printf("Error parsing request body: %v\n", err)
		http.Error(w, "Error parsing request body", http.StatusBadRequest)
		return
	}

	maxTokens := shared.MaxContextTokens
	tokensDiff := 0
	totalTokens := branch.ContextTokens
	tokensDiffById := make(map[string]int)
	contextsById := make(map[string]*db.Context)
	var updatedContexts []*shared.Context

	numFiles := 0
	numUrls := 0
	numTrees := 0

	var mu sync.Mutex
	errCh := make(chan error)

	for id, params := range requestBody {
		go func(id string, params *shared.UpdateContextParams) {
			context, err := db.GetContext(auth.OrgId, planId, id, true)

			if err != nil {
				errCh <- fmt.Errorf("error getting context: %v", err)
				return
			}

			// spew.Dump(context)

			mu.Lock()
			defer mu.Unlock()

			contextsById[id] = context
			updatedContexts = append(updatedContexts, context.ToApi())
			updateNumTokens, err := shared.GetNumTokens(params.Body)

			if err != nil {
				errCh <- fmt.Errorf("error getting num tokens: %v", err)
				return
			}

			tokenDiff := updateNumTokens - context.NumTokens
			tokensDiffById[id] = tokenDiff
			tokensDiff += tokenDiff
			totalTokens += tokenDiff

			context.NumTokens = updateNumTokens

			switch context.ContextType {
			case shared.ContextFileType:
				numFiles++
			case shared.ContextURLType:
				numUrls++
			case shared.ContextDirectoryTreeType:
				numTrees++
			}

			errCh <- nil
		}(id, params)
	}

	for i := 0; i < len(requestBody); i++ {
		err := <-errCh
		if err != nil {
			log.Printf("Error updating context: %v\n", err)
			http.Error(w, "Error updating context: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if totalTokens > maxTokens {
		log.Printf("The total number of tokens (%d) exceeds the maximum allowed (%d)", totalTokens, maxTokens)
		res := shared.LoadContextResponse{
			TokensAdded:       tokensDiff,
			TotalTokens:       totalTokens,
			MaxTokensExceeded: true,
		}

		bytes, err := json.Marshal(res)

		if err != nil {
			log.Printf("Error marshalling response: %v\n", err)
			http.Error(w, "Error marshalling response: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Write(bytes)
		return
	}

	unlockFn := lockRepo(w, r, auth, db.LockScopeWrite)
	if unlockFn == nil {
		return
	} else {
		defer (*unlockFn)(err)
	}

	errCh = make(chan error)

	for id, params := range requestBody {
		go func(id string, params *shared.UpdateContextParams) {

			context := contextsById[id]

			hash := sha256.Sum256([]byte(params.Body))
			sha := hex.EncodeToString(hash[:])

			context.Body = params.Body
			context.Sha = sha

			errCh <- db.StoreContext(context)
		}(id, params)
	}

	for i := 0; i < len(requestBody); i++ {
		err := <-errCh
		if err != nil {
			log.Printf("Error creating context: %v\n", err)
			http.Error(w, "Error creating context: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	updateRes := &shared.ContextUpdateResult{
		UpdatedContexts: updatedContexts,
		TokenDiffsById:  tokensDiffById,
		TokensDiff:      tokensDiff,
		TotalTokens:     totalTokens,
		NumFiles:        numFiles,
		NumUrls:         numUrls,
		NumTrees:        numTrees,
	}

	commitMsg := shared.SummaryForUpdateContext(updateRes) + "\n\n" + shared.TableForContextUpdate(updateRes)
	err = db.GitAddAndCommit(auth.OrgId, planId, branchName, commitMsg)

	if err != nil {
		log.Printf("Error committing changes: %v\n", err)
		http.Error(w, "Error committing changes: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = db.AddPlanContextTokens(planId, branchName, tokensDiff)
	if err != nil {
		log.Printf("Error updating plan tokens: %v\n", err)
		http.Error(w, "Error updating plan tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res := shared.UpdateContextResponse{
		TokensAdded: tokensDiff,
		TotalTokens: totalTokens,
		Msg:         commitMsg,
	}

	bytes, err := json.Marshal(res)

	if err != nil {
		log.Printf("Error marshalling response: %v\n", err)
		http.Error(w, "Error marshalling response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("Successfully processed UpdateContextHandler request")

	w.Write(bytes)
}

func DeleteContextHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received request for DeleteContextHandler")

	auth := authenticate(w, r, true)
	if auth == nil {
		return
	}

	vars := mux.Vars(r)
	planId := vars["planId"]
	branchName := vars["branch"]
	log.Println("planId: ", planId)

	plan := authorizePlan(w, planId, auth)

	if plan == nil {
		return
	}

	branch, err := db.GetDbBranch(planId, branchName)

	// read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var requestBody shared.DeleteContextRequest
	if err := json.Unmarshal(body, &requestBody); err != nil {
		log.Printf("Error parsing request body: %v\n", err)
		http.Error(w, "Error parsing request body", http.StatusBadRequest)
		return
	}

	unlockFn := lockRepo(w, r, auth, db.LockScopeWrite)
	if unlockFn == nil {
		return
	} else {
		defer (*unlockFn)(err)
	}

	dbContexts, err := db.GetPlanContexts(auth.OrgId, planId, false)

	if err != nil {
		log.Printf("Error getting contexts: %v\n", err)
		http.Error(w, "Error getting contexts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var toRemove []*db.Context
	for _, dbContext := range dbContexts {
		if _, ok := requestBody.Ids[dbContext.Id]; ok {
			toRemove = append(toRemove, dbContext)
		}
	}

	err = db.ContextRemove(toRemove)

	if err != nil {
		log.Printf("Error deleting contexts: %v\n", err)
		http.Error(w, "Error deleting contexts: "+err.Error(), http.StatusInternalServerError)
		return
	}

	removeTokens := 0
	var toRemoveApiContexts []*shared.Context
	for _, dbContext := range toRemove {
		toRemoveApiContexts = append(toRemoveApiContexts, dbContext.ToApi())
		removeTokens += dbContext.NumTokens
	}

	commitMsg := shared.SummaryForRemoveContext(toRemoveApiContexts, branch.ContextTokens) + "\n\n" + shared.TableForRemoveContext(toRemoveApiContexts)
	err = db.GitAddAndCommit(auth.OrgId, planId, branchName, commitMsg)

	if err != nil {
		log.Printf("Error committing changes: %v\n", err)
		http.Error(w, "Error committing changes: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = db.AddPlanContextTokens(planId, branchName, -removeTokens)
	if err != nil {
		log.Printf("Error updating plan tokens: %v\n", err)
		http.Error(w, "Error updating plan tokens: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res := shared.DeleteContextResponse{
		TokensRemoved: removeTokens,
		TotalTokens:   branch.ContextTokens - removeTokens,
		Msg:           commitMsg,
	}

	bytes, err := json.Marshal(res)

	if err != nil {
		log.Printf("Error marshalling response: %v\n", err)
		http.Error(w, "Error marshalling response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Println("Successfully deleted contexts")

	w.Write(bytes)
}
