// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import git_model "gitea.dev/models/git"

// GetGrantedApprovalsCountFromReviews applies the same approval predicates as
// GetGrantedApprovalsCount to reviews already loaded by the caller.
func GetGrantedApprovalsCountFromReviews(protectBranch *git_model.ProtectedBranch, reviews ReviewList) int64 {
	if protectBranch == nil {
		return 0
	}

	var approvals int64
	for _, review := range reviews {
		if review == nil {
			continue
		}
		if review.Type != ReviewTypeApprove || !review.Official || review.Dismissed {
			continue
		}
		if protectBranch.IgnoreStaleApprovals && review.Stale {
			continue
		}
		approvals++
	}
	return approvals
}

// HasEnoughApprovalsFromReviews applies the same approval gate as
// HasEnoughApprovals to reviews already loaded by the caller.
func HasEnoughApprovalsFromReviews(protectBranch *git_model.ProtectedBranch, reviews ReviewList) bool {
	return protectBranch != nil && (protectBranch.RequiredApprovals == 0 ||
		GetGrantedApprovalsCountFromReviews(protectBranch, reviews) >= protectBranch.RequiredApprovals)
}

// MergeBlockedByRejectedReviewFromReviews applies the same rejection gate as
// MergeBlockedByRejectedReview to reviews already loaded by the caller.
func MergeBlockedByRejectedReviewFromReviews(protectBranch *git_model.ProtectedBranch, reviews ReviewList) bool {
	if protectBranch == nil || !protectBranch.BlockOnRejectedReviews {
		return false
	}
	for _, review := range reviews {
		if review == nil {
			continue
		}
		if review.Type == ReviewTypeReject && review.Official && !review.Dismissed {
			return true
		}
	}
	return false
}

// MergeBlockedByOfficialReviewRequestsFromReviews applies the same official
// review-request gate as MergeBlockedByOfficialReviewRequests to reviews
// already loaded by the caller.
func MergeBlockedByOfficialReviewRequestsFromReviews(protectBranch *git_model.ProtectedBranch, reviews ReviewList) bool {
	if protectBranch == nil || !protectBranch.BlockOnOfficialReviewRequests {
		return false
	}
	for _, review := range reviews {
		if review == nil {
			continue
		}
		if review.Type == ReviewTypeRequest && review.Official && !review.Dismissed {
			return true
		}
	}
	return false
}
