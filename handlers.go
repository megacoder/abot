package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"

	"github.com/avabot/ava/Godeps/_workspace/src/github.com/labstack/echo"
	mw "github.com/avabot/ava/Godeps/_workspace/src/github.com/labstack/echo/middleware"
	"github.com/avabot/ava/Godeps/_workspace/src/golang.org/x/crypto/bcrypt"
	"github.com/satori/go.uuid"
	"github.com/stripe/stripe-go"
	"github.com/stripe/stripe-go/customer"
)

func initRoutes(e *echo.Echo) {
	e.Use(mw.Logger(), mw.Gzip(), mw.Recover())
	e.SetDebug(true)

	e.Static("/public/css", "assets/css")
	e.Static("/public/js", "public/js")
	e.Static("/public/images", "assets/images")

	// Web routes
	e.Get("/", handlerIndex)

	// API routes
	e.Post("/", handlerMain)
	e.Post("/twilio", handlerTwilio)
	e.Get("/api/sentence.json", handlerAPISentence)
	e.Put("/api/sentence.json", handlerAPITrainSentence)
	e.Post("/api/login.json", handlerAPILoginSubmit)
	e.Post("/api/signup.json", handlerAPISignupSubmit)
	e.Post("/api/cards.json", handlerAPICardSubmit)
}

func handlerIndex(c *echo.Context) error {
	tmplLayout, err := template.ParseFiles("assets/html/layout.html")
	if err != nil {
		log.Fatalln(err)
	}
	tmplIndex, err := template.ParseFiles("assets/html/index.html")
	if err != nil {
		log.Fatalln(err)
	}
	var s []byte
	b := bytes.NewBuffer(s)
	data := struct {
		StripeKey string
	}{StripeKey: os.Getenv("STRIPE_PUBLIC_KEY")}
	if err := tmplIndex.Execute(b, data); err != nil {
		log.Fatalln(err)
	}
	b2 := bytes.NewBuffer(s)
	if err := tmplLayout.Execute(b2, b); err != nil {
		log.Fatalln(err)
	}
	if err = c.HTML(http.StatusOK, "%s", b2); err != nil {
		return err
	}
	return nil
}

func handlerTwilio(c *echo.Context) error {
	c.Set("cmd", c.Form("Body"))
	c.Set("flexid", c.Form("From"))
	c.Set("flexidtype", 2)
	ret, err := processText(c)
	if err != nil {
		return err
	}
	var resp twilioResp
	if len(ret) == 0 {
		resp = twilioResp{}
	} else {
		resp = twilioResp{Message: ret}
	}
	if err = c.XML(http.StatusOK, resp); err != nil {
		return err
	}
	return nil
}

func handlerAPISentence(c *echo.Context) error {
	var q string
	var sent struct {
		ID             int
		ForeignID      string
		Sentence       string
		MaxAssignments int
	}
	if len(c.Query("id")) > 0 {
		q = `
		SELECT id, foreignid, sentence, maxassignments FROM trainings
		WHERE trainedcount<3 AND id=$1
		OFFSET FLOOR(RANDOM() * (SELECT COUNT(*) FROM trainings WHERE trainedcount<3 AND id=$1))`
		err := db.Get(&sent, q, c.Query("id"))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	} else {
		q = `
		SELECT id, foreignid, sentence, maxassignments FROM trainings
		WHERE trainedcount<3
		OFFSET FLOOR(RANDOM() * (SELECT COUNT(*) FROM trainings WHERE trainedcount<3))`
		err := db.Get(&sent, q)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	if err := c.JSON(http.StatusOK, sent); err != nil {
		return err
	}
	return nil
}

func handlerAPITrainSentence(c *echo.Context) error {
	var data TrainingData
	if err := c.Bind(&data); err != nil {
		return err
	}
	if err := train(bayes, data.Sentence); err != nil {
		return err
	}
	q := `UPDATE trainings SET trainedcount=trainedcount+1 WHERE id=$1`
	res, err := db.Exec(q, data.ID)
	if err != nil {
		return err
	}
	num, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if num == 0 {
		return sql.ErrNoRows
	}
	if err = checkConsensus(&data); err != nil {
		return err
	}
	if err = c.JSON(http.StatusOK, nil); err != nil {
		return err
	}
	return nil
}

func handlerMain(c *echo.Context) error {
	c.Set("cmd", c.Form("cmd"))
	c.Set("flexid", c.Form("flexid"))
	c.Set("flexidtype", c.Form("flexidtype"))
	c.Set("uid", c.Form("uid"))
	ret, err := processText(c)
	if err != nil {
		return err
	}
	if err = c.HTML(http.StatusOK, ret); err != nil {
		return err
	}
	return nil
}

func handlerAPILoginSubmit(c *echo.Context) error {
	var u struct {
		Id       int
		Password []byte
	}
	var req struct {
		Email    string
		Password string
	}
	if err := c.Bind(&req); err != nil {
		return jsonError(err)
	}
	q := `SELECT id, password FROM users WHERE email=$1`
	resp := struct {
		Id           int
		SessionToken string
	}{}
	err := db.Get(&u, q, req.Email)
	if err == sql.ErrNoRows {
		return jsonError(ErrInvalidUserPass)
	} else if err != nil {
		return jsonError(err)
	}
	if u.Id == 0 {
		return jsonError(ErrInvalidUserPass)
	}
	err = bcrypt.CompareHashAndPassword(u.Password, []byte(req.Password))
	if err == bcrypt.ErrMismatchedHashAndPassword ||
		err == bcrypt.ErrHashTooShort {
		return jsonError(ErrInvalidUserPass)
	} else if err != nil {
		return jsonError(err)
	}
	resp.Id = u.Id
	tmp := uuid.NewV4()
	if err != nil {
		return jsonError(err)
	}
	resp.SessionToken = base64.StdEncoding.EncodeToString(tmp.Bytes())
	// TODO save session token
	if err = c.JSON(http.StatusOK, resp); err != nil {
		return jsonError(err)
	}
	return nil
}

func handlerAPISignupSubmit(c *echo.Context) error {
	req := struct {
		Name     string
		Email    string
		Password string
		FID      string
	}{}
	if err := c.Bind(&req); err != nil {
		return jsonError(err)
	}
	if len(req.Name) == 0 {
		return jsonError(errors.New("You must enter a name."))
	}
	if len(req.Email) == 0 || !strings.ContainsAny(req.Email, "@") ||
		!strings.ContainsAny(req.Email, ".") {
		return jsonError(errors.New("You must enter a valid email."))
	}
	if len(req.Password) < 8 {
		return jsonError(errors.New(
			"Your password must be at least 8 characters."))
	}
	if err := validatePhone(req.FID); err != nil {
		return jsonError(err)
	}
	// TODO format phone number for Twilio (international format)
	hpw, err := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
	if err != nil {
		return jsonError(err)
	}
	customerParams := &stripe.CustomerParams{Email: req.Email}
	cust, err := customer.New(customerParams)
	if err != nil {
		return jsonError(err)
	}
	tx, err := db.Beginx()
	if err != nil {
		return jsonError(errors.New("Something went wrong. Try again."))
	}
	q := `INSERT INTO users (name, email, password, locationid, stripecustomerid)
	     VALUES ($1, $2, $3, 0, $4)
	     RETURNING id`
	var uid int
	err = tx.QueryRowx(q, req.Name, req.Email, hpw, cust.ID).Scan(&uid)
	if err != nil && err.Error() ==
		`pq: duplicate key value violates unique constraint "users_email_key"` {
		_ = tx.Rollback()
		return jsonError(errors.New("Sorry, that email is taken."))
	}
	if uid == 0 {
		_ = tx.Rollback()
		return jsonError(errors.New(
			"Something went wrong. Please try again."))
	}
	q = `INSERT INTO userflexids (userid, flexid, flexidtype)
	     VALUES ($1, $2, $3)`
	_, err = tx.Exec(q, uid, req.FID, 2)
	if err != nil {
		_ = tx.Rollback()
		return jsonError(errors.New(
			"Couldn't sign up. Did you use the link sent to you?"))
	}
	if err = tx.Commit(); err != nil {
		return jsonError(errors.New(
			"Something went wrong. Please try again."))
	}
	resp := struct {
		Id           int
		SessionToken string
	}{}
	tmp := uuid.NewV4()
	resp.SessionToken = base64.StdEncoding.EncodeToString(tmp.Bytes())
	// TODO save session token
	if err = c.JSON(http.StatusOK, resp); err != nil {
		return jsonError(err)
	}
	return nil
}

func handlerAPIProfile(c *echo.Context) error {
	uid, err := strconv.Atoi(c.Query("uid"))
	if err != nil {
		return jsonError(err)
	}
	var user struct {
		Name   string
		Email  string
		Phones []struct {
			Id     int
			Number string `db:"flexid"`
		}
		Cards []struct {
			Id       int
			Last4    string
			ExpMonth string `db:"expmonth"`
			ExpYear  string `db:"expyear"`
			Brand    string
		}
		Addresses []struct {
			Id      int
			Name    string
			Line1   string
			Line2   string
			City    string
			State   string
			Country string
			Zip     string
		}
	}
	q := `SELECT name, email FROM users WHERE id=$1`
	err = db.Get(&user, q, uid)
	if err != nil {
		return jsonError(err)
	}
	q = `
		SELECT id, flexid FROM userflexids
		WHERE flexidtype=2 AND userid=$1
		LIMIT 10`
	err = db.Select(&user.Phones, q, uid)
	if err != nil && err != sql.ErrNoRows {
		return jsonError(err)
	}
	q = `
		SELECT id, name, last4, expmonth, expyear, brand
		FROM cards
		WHERE userid=$1
		LIMIT 10`
	err = db.Select(&user.Cards, q, uid)
	if err != nil && err != sql.ErrNoRows {
		return jsonError(err)
	}
	q = `
		SELECT id, name, line1, line2, city, state, country, zip
		FROM addresses
		WHERE userid=$1
		LIMIT 10`
	err = db.Select(&user.Addresses, q, uid)
	if err != nil && err != sql.ErrNoRows {
		return jsonError(err)
	}
	if err = c.JSON(http.StatusOK, user); err != nil {
		return jsonError(err)
	}
	return nil
}

func handlerAPICardSubmit(c *echo.Context) error {
	var req struct {
		StripeID       string
		CardholderName string
		Last4          string
		Brand          string
		Fingerprint    string
		ExpMonth       int
		ExpYear        int
		UserID         int
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	var cid struct{ ID int }
	q := `
		INSERT INTO cards
		(userid, last4, cardholdername, expmonth, expyear, brand,
			stripeid, fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`
	err := db.QueryRowx(q, req.UserID, req.Last4, req.CardholderName,
		req.ExpMonth, req.ExpYear, req.Brand, req.StripeID,
		req.Fingerprint).Scan(&cid.ID)
	if err != nil {
		return jsonError(err)
	}
	if err = c.JSON(http.StatusOK, cid); err != nil {
		return jsonError(err)
	}
	return nil
}

func jsonError(err error) error {
	return errors.New(`{"Msg":"` + err.Error() + `"}`)
}

func validatePhone(s string) error {
	if len(s) < 10 || len(s) > 20 ||
		!phoneRegex.MatchString(s) {
		return errors.New(
			"Your phone must be a valid U.S. number with the area code.")
	}
	if len(s) == 11 && s[0] != '1' {
		return errors.New(
			"Sorry, Ava only serves U.S. customers for now.")
	}
	if len(s) == 12 && s[0] == '+' && s[1] != '1' {
		return errors.New(
			"Sorry, Ava only serves U.S. customers for now.")
	}
	return nil
}