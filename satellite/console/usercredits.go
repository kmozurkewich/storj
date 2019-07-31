// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package console

import (
	"context"
	"time"

	"github.com/skyrings/skyring-common/tools/uuid"
	"github.com/zeebo/errs"

	"storj.io/storj/internal/currency"
)

// NoCreditForUpdateErr is a error message used when no credits are found for update when new users sign up
var NoCreditForUpdateErr = errs.Class("no credit found to update")

// UserCredits holds information to interact with database
type UserCredits interface {
	GetCreditUsage(ctx context.Context, userID uuid.UUID, expirationEndDate time.Time) (*UserCreditUsage, error)
	Create(ctx context.Context, userCredit UserCredit) error
	UpdateEarnedCredits(ctx context.Context, userID uuid.UUID) error
	UpdateAvailableCredits(ctx context.Context, creditsToCharge int, id uuid.UUID, billingStartDate time.Time) (remainingCharge int, err error)
}

// UserCredit holds information about an user's credit
type UserCredit struct {
	ID            int
	UserID        uuid.UUID
	OfferID       int
	ReferredBy    *uuid.UUID
	CreditsEarned currency.USD
	CreditsUsed   currency.USD
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// UserCreditUsage holds information about credit usage information
type UserCreditUsage struct {
	Referred         int64
	AvailableCredits currency.USD
	UsedCredits      currency.USD
}
