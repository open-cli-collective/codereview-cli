package reportidentity

import (
	"fmt"
	"sort"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

const (
	statusComplete   = "complete"
	statusUnresolved = "unresolved"
	statusError      = "error"
)

func enumeratePairs(records Records) ([]*expectedPair, map[pairKey]*expectedPair, map[string]int, map[string]int, error) {
	findingIndexes := make(map[string]int, len(records.Findings))
	for index, finding := range records.Findings {
		findingIndexes[string(finding.ID)] = index
	}
	threadIndexes := make(map[string]int, len(records.Threads))
	for index, thread := range records.Threads {
		threadIndexes[thread.ID] = index
	}

	pairCount := countExpectedPairs(records.Task, len(records.Findings), len(records.Threads))
	pairs := make([]*expectedPair, 0, pairCount)
	byKey := make(map[pairKey]*expectedPair, pairCount)
	add := func(leftIndex, rightIndex int, leftID, rightID string, left, right any) error {
		digest, err := pairDigest(records.Task, left, right)
		if err != nil {
			return err
		}
		pair := &expectedPair{
			leftIndex:   leftIndex,
			rightIndex:  rightIndex,
			leftID:      leftID,
			rightID:     rightID,
			inputSHA256: digest,
		}
		key := pairKey{left: leftID, right: rightID}
		pairs = append(pairs, pair)
		byKey[key] = pair
		return nil
	}

	if records.Task == TaskCohort {
		for leftIndex := 0; leftIndex < len(records.Findings); leftIndex++ {
			for rightIndex := leftIndex + 1; rightIndex < len(records.Findings); rightIndex++ {
				left := records.Findings[leftIndex]
				right := records.Findings[rightIndex]
				if err := add(leftIndex, rightIndex, string(left.ID), string(right.ID), left, right); err != nil {
					return nil, nil, nil, nil, err
				}
			}
		}
		return pairs, byKey, findingIndexes, threadIndexes, nil
	}

	for findingIndex, finding := range records.Findings {
		for threadIndex, thread := range records.Threads {
			if !thread.Eligible {
				continue
			}
			if err := add(findingIndex, threadIndex, string(finding.ID), thread.ID, finding, thread); err != nil {
				return nil, nil, nil, nil, err
			}
		}
	}
	return pairs, byKey, findingIndexes, threadIndexes, nil
}

func normalizeDecisionKey(task Task, decision Decision, findingIndexes, threadIndexes map[string]int, expected map[pairKey]*expectedPair) (pairKey, error) {
	if err := validateID(decision.LeftID, "decision.left_id"); err != nil {
		return pairKey{}, err
	}
	if err := validateID(decision.RightID, "decision.right_id"); err != nil {
		return pairKey{}, err
	}
	if decision.LeftID == decision.RightID {
		return pairKey{}, fmt.Errorf("self-pairs are not allowed")
	}

	key := pairKey{left: decision.LeftID, right: decision.RightID}
	if task == TaskCohort {
		leftIndex, leftFound := findingIndexes[decision.LeftID]
		rightIndex, rightFound := findingIndexes[decision.RightID]
		if !leftFound || !rightFound {
			return pairKey{}, fmt.Errorf("pair %q/%q is not an expected cohort pair", decision.LeftID, decision.RightID)
		}
		if leftIndex > rightIndex {
			key.left, key.right = key.right, key.left
		}
	} else {
		_, findingFound := findingIndexes[decision.LeftID]
		_, threadFound := threadIndexes[decision.RightID]
		if !findingFound || !threadFound {
			return pairKey{}, fmt.Errorf("pair %q/%q is not an expected prior-thread pair", decision.LeftID, decision.RightID)
		}
	}
	if _, ok := expected[key]; !ok {
		if task == TaskPriorThread {
			if _, findingFound := findingIndexes[decision.LeftID]; findingFound {
				if _, threadFound := threadIndexes[decision.RightID]; threadFound {
					return pairKey{}, fmt.Errorf("prior thread %q is ineligible for identity comparison", decision.RightID)
				}
			}
		}
		return pairKey{}, fmt.Errorf("pair %q/%q is not expected for this task", decision.LeftID, decision.RightID)
	}
	return key, nil
}

func assembleCohort(result *Result, findings []review.Finding, expected []*expectedPair) {
	sets := newDisjointSet(len(findings))
	for _, pair := range expected {
		if pair.decisionSeen && pair.choice == ChoiceSame {
			sets.union(pair.leftIndex, pair.rightIndex)
		}
	}

	hasError := false
	hasUnresolved := false
	hasContradiction := false
	for _, pair := range expected {
		var code string
		switch {
		case !pair.decisionSeen:
			code = "missing"
			hasUnresolved = true
		case pair.choice == ChoiceAbstain:
			code = "abstain"
			hasUnresolved = true
		case pair.choice == ChoiceError:
			code = "error"
			hasError = true
		case pair.choice == ChoiceDifferent && sets.find(pair.leftIndex) == sets.find(pair.rightIndex):
			code = "contradiction"
			hasContradiction = true
			hasUnresolved = true
		}
		if code != "" {
			result.Issues = append(result.Issues, Issue{Code: code, LeftID: pair.leftID, RightID: pair.rightID})
		}
	}

	switch {
	case hasError:
		result.Status = statusError
	case hasUnresolved || hasContradiction:
		result.Status = statusUnresolved
	default:
		result.Status = statusComplete
	}

	if result.Status != statusComplete {
		result.Groups = singletonGroups(findings)
		result.RollupOrderedFindings = orderFindings(findings, allFindingIndexes(len(findings)))
		return
	}

	componentIndexes := make(map[int][]int, len(findings))
	componentOrder := make([]int, 0, len(findings))
	for index := range findings {
		root := sets.find(index)
		if _, exists := componentIndexes[root]; !exists {
			componentOrder = append(componentOrder, root)
		}
		componentIndexes[root] = append(componentIndexes[root], index)
	}

	representativeIndexes := make([]int, 0, len(componentOrder))
	for _, root := range componentOrder {
		members := componentIndexes[root]
		representative := chooseRepresentative(findings, members)
		representativeIndexes = append(representativeIndexes, representative)
		memberIDs := make([]string, 0, len(members))
		dropped := make([]review.FindingID, 0, len(members)-1)
		for _, index := range members {
			id := string(findings[index].ID)
			memberIDs = append(memberIDs, id)
			if index != representative {
				dropped = append(dropped, findings[index].ID)
			}
		}
		result.Groups = append(result.Groups, Group{
			RepresentativeID: string(findings[representative].ID),
			MemberIDs:        memberIDs,
		})
		if len(dropped) > 0 {
			result.RollupDedupeLog = append(result.RollupDedupeLog, review.DedupeEntry{
				Kept:    findings[representative].ID,
				Dropped: dropped,
				Reason:  "Recorded identity match",
			})
		}
	}
	result.RollupOrderedFindings = orderFindings(findings, representativeIndexes)
}

func assemblePriorThreads(result *Result, expected []*expectedPair) {
	hasError := false
	hasUnresolved := false
	for _, pair := range expected {
		if !pair.decisionSeen {
			result.Issues = append(result.Issues, Issue{Code: "missing", LeftID: pair.leftID, RightID: pair.rightID})
			hasUnresolved = true
			continue
		}
		switch pair.choice {
		case ChoiceSame:
			result.Associations = append(result.Associations, Association{FindingID: pair.leftID, ThreadID: pair.rightID})
		case ChoiceAbstain:
			result.Issues = append(result.Issues, Issue{Code: "abstain", LeftID: pair.leftID, RightID: pair.rightID})
			hasUnresolved = true
		case ChoiceError:
			result.Issues = append(result.Issues, Issue{Code: "error", LeftID: pair.leftID, RightID: pair.rightID})
			hasError = true
		case ChoiceDifferent:
			// Different findings do not create a prior-thread association.
		case ChoiceMissing:
			result.Issues = append(result.Issues, Issue{Code: "missing", LeftID: pair.leftID, RightID: pair.rightID})
			hasUnresolved = true
		}
	}
	switch {
	case hasError:
		result.Status = statusError
	case hasUnresolved:
		result.Status = statusUnresolved
	default:
		result.Status = statusComplete
	}
}

func singletonGroups(findings []review.Finding) []Group {
	groups := make([]Group, 0, len(findings))
	for _, finding := range findings {
		id := string(finding.ID)
		groups = append(groups, Group{RepresentativeID: id, MemberIDs: []string{id}})
	}
	return groups
}

func chooseRepresentative(findings []review.Finding, members []int) int {
	best := members[0]
	ranks := severityRanks()
	for _, index := range members[1:] {
		if ranks[findings[index].Severity] < ranks[findings[best].Severity] {
			best = index
		}
	}
	return best
}

func orderFindings(findings []review.Finding, indexes []int) []review.FindingID {
	ranks := severityRanks()
	ordered := append([]int(nil), indexes...)
	sort.SliceStable(ordered, func(left, right int) bool {
		leftRank := ranks[findings[ordered[left]].Severity]
		rightRank := ranks[findings[ordered[right]].Severity]
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return ordered[left] < ordered[right]
	})
	ids := make([]review.FindingID, 0, len(ordered))
	for _, index := range ordered {
		ids = append(ids, findings[index].ID)
	}
	return ids
}

func severityRanks() map[review.Severity]int {
	order := review.SeverityOrder()
	ranks := make(map[review.Severity]int, len(order))
	for index, severity := range order {
		ranks[severity] = index
	}
	return ranks
}

func allFindingIndexes(count int) []int {
	indexes := make([]int, count)
	for index := range indexes {
		indexes[index] = index
	}
	return indexes
}

type disjointSet struct {
	parents []int
	ranks   []int
}

func newDisjointSet(size int) *disjointSet {
	parents := make([]int, size)
	for index := range parents {
		parents[index] = index
	}
	return &disjointSet{parents: parents, ranks: make([]int, size)}
}

func (sets *disjointSet) find(value int) int {
	if sets.parents[value] != value {
		sets.parents[value] = sets.find(sets.parents[value])
	}
	return sets.parents[value]
}

func (sets *disjointSet) union(left, right int) {
	leftRoot := sets.find(left)
	rightRoot := sets.find(right)
	if leftRoot == rightRoot {
		return
	}
	if sets.ranks[leftRoot] < sets.ranks[rightRoot] {
		leftRoot, rightRoot = rightRoot, leftRoot
	}
	sets.parents[rightRoot] = leftRoot
	if sets.ranks[leftRoot] == sets.ranks[rightRoot] {
		sets.ranks[leftRoot]++
	}
}
