package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	idx "github.com/okta/okta-idx-golang"
)

// BEGIN: Login
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.cache.Delete("loginResponse")
	// Initialize the login so we can see if there are Social IDP's to display
	lr, err := s.idxClient.InitLogin(r.Context())
	if err != nil {
		log.Fatalf("Could not initalize login: %s", err.Error())
	}

	// Store the login response in cache to use in the handler
	s.cache.Set("loginResponse", lr, time.Minute*5)

	// Set IDP's in the ViewData to iterate over.
	idps := lr.IdentityProviders()
	s.ViewData["IDPs"] = idps
	s.ViewData["IdpCount"] = func() int {
		return len(idps)
	}

	// Render the login page
	s.render("login.gohtml", w, r)
}

// logout revokes the oauth2 token server side
func (s *Server) logout(r *http.Request) {
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil || session.Values["access_token"] == nil || session.Values["access_token"] == "" {
		return
	}

	var revokeTokenUrl string
	issuer := s.idxClient.Config().Okta.IDX.Issuer
	if strings.Contains(issuer, "oauth2") {
		revokeTokenUrl = issuer + "/v1/revoke"
	} else {
		revokeTokenUrl = issuer + "/oauth2/v1/revoke"
	}

	form := url.Values{}
	form.Set("token", session.Values["access_token"].(string))
	form.Set("token_type_hint", "access_token")
	form.Set("client_id", s.idxClient.Config().Okta.IDX.ClientID)
	form.Set("client_secret", s.idxClient.Config().Okta.IDX.ClientSecret)
	req, _ := http.NewRequest("POST", revokeTokenUrl, strings.NewReader(form.Encode()))
	h := req.Header
	h.Add("Accept", "application/json")
	h.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: time.Second * 30}
	resp, err := client.Do(req)
	if err != nil {
		body, _ := ioutil.ReadAll(resp.Body)
		fmt.Printf("revoke error; status: %s, body: %s\n", resp.Status, string(body))
	}
	defer resp.Body.Close()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	s.cache.Delete("loginResponse")
	lr := clr.(*idx.LoginResponse)

	// PUll data from the web form and create your identify request
	// THis is used in the Identify step
	ir := &idx.IdentifyRequest{
		Identifier: r.FormValue("identifier"),
		Credentials: idx.Credentials{
			Password: r.FormValue("password"),
		},
	}

	// Get session store so we can store our tokens
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}

	lr, err = lr.Identify(r.Context(), ir)
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginSecondaryFactors(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Deal with there aren't any login steps, perhaps user didn't complete enrollment.
	if len(lr.AvailableSteps()) == 0 ||
		(len(lr.AvailableSteps()) == 1 && lr.HasStep(idx.LoginStepCancel)) {
		session, err := sessionStore.Get(r, "direct-auth")
		if err == nil {
			session.Values["Errors"] = "There should be should be additional login factors available but they are not."
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	s.ViewData["FactorEmail"] = lr.HasStep(idx.LoginStepEmailVerification)
	s.ViewData["FactorPhone"] = lr.HasStep(idx.LoginStepPhoneVerification) || lr.HasStep(idx.LoginStepPhoneInitialVerification)
	s.ViewData["FactorGoogleAuth"] = lr.HasStep(idx.LoginStepGoogleAuthenticatorInitialVerification) || lr.HasStep(idx.LoginStepGoogleAuthenticatorConfirmation)
	s.ViewData["FactorOktaVerify"] = lr.HasStep(idx.LoginStepOktaVerify)
	s.ViewData["FactorSkip"] = lr.HasStep(idx.LoginStepSkip)
	s.ViewData["FactorWebAuthN"] = lr.HasStep(idx.LoginStepWebAuthNSetup) || lr.HasStep(idx.LoginStepWebAuthNChallenge)
	s.ViewData["FactorSecurityQuestion"] = lr.HasStep(idx.LoginStepSecurityQuestionOptions)

	s.render("loginSecondaryFactors.gohtml", w, r)
}

func (s *Server) handleLoginSecondaryFactorsProceed(w http.ResponseWriter, r *http.Request) {
	delete(s.ViewData, "InvalidEmailCode")
	submit := r.FormValue("submit")
	if submit == "Skip" {
		clr, _ := s.cache.Get("loginResponse")
		lr := clr.(*idx.LoginResponse)
		s.loginTransitionToProfile(lr, w, r)
		return
	}
	pushFactor := r.FormValue("push_factor")
	switch pushFactor {
	case "push_email":
		http.Redirect(w, r, "/login/factors/email", http.StatusFound)
		return
	case "push_phone":
		http.Redirect(w, r, "/login/factors/phone/method", http.StatusFound)
		return
	case "push_okta_verify":
		http.Redirect(w, r, "/login/factors/okta-verify", http.StatusFound)
		return
	case "push_google_auth":
		http.Redirect(w, r, "/login/factors/google_auth", http.StatusFound)
		return
	case "push_web_authn":
		http.Redirect(w, r, "/login/factors/web_authn", http.StatusFound)
		return
	case "push_security_question":
		http.Redirect(w, r, "/login/factors/security_question", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) loginTransitionToProfile(er *idx.LoginResponse, w http.ResponseWriter, r *http.Request) {
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}

	lr, err := er.Skip(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)

	if lr.Token() != nil {
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginEmailVerification(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepEmailVerification) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	invCode, ok := s.ViewData["InvalidEmailCode"]
	if !ok || !invCode.(bool) {
		lr, err := lr.VerifyEmail(r.Context())
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		s.cache.Set("loginResponse", lr, time.Minute*5)
	}

	// set the idx state string in the session for inspection for otp login callback comparison.
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	session.Values["idxContext.state"] = lr.Context().State
	err = session.Save(r, w)
	if err != nil {
		log.Fatalf("could not save idx context state: %s", err)
	}

	s.render("loginFactorEmail.gohtml", w, r)
}

func (s *Server) handleLoginEmailConfirmation(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepEmailConfirmation) {
		http.Redirect(w, r, "login/", http.StatusFound)
		return
	}
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	lr, err = lr.ConfirmEmail(r.Context(), r.FormValue("code"))
	if err != nil {
		s.ViewData["InvalidEmailCode"] = true
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/email", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	s.ViewData["InvalidEmailCode"] = false

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginSecurityQuestion(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepSecurityQuestionOptions) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	lr, questions, err := lr.SecurityQuestionOptions(r.Context())
	if err != nil {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	s.ViewData["Questions"] = questions
	s.render("loginSetupSecurityQuestion.gohtml", w, r)
}

func (s *Server) handleLoginSecurityQuestionSetup(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepSecurityQuestionSetup) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}

	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}

	sq := idx.SecurityQuestion{
		QuestionKey: r.FormValue("question"),
		Question:    r.FormValue("custom_question"),
		Answer:      r.FormValue("answer"),
	}
	lr, err = lr.SecurityQuestionSetup(r.Context(), &sq)
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/security_question", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginPhoneVerificationMethod(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if lr.HasStep(idx.LoginStepPhoneInitialVerification) || lr.HasStep(idx.LoginStepPhoneVerification) {
		if lr.HasStep(idx.LoginStepPhoneInitialVerification) {
			s.ViewData["InitialPhoneSetup"] = true
		} else {
			s.ViewData["InitialPhoneSetup"] = false
		}
		s.render("loginFactorPhoneMethod.gohtml", w, r)
		return
	}
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginPhoneVerification(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	if lr.HasStep(idx.LoginStepPhoneInitialVerification) || lr.HasStep(idx.LoginStepPhoneVerification) {
		// get method
		_ = r.FormValue("voice")
		_ = r.FormValue("sms")
		invCode, ok := s.ViewData["InvalidPhoneCode"]
		if !ok || !invCode.(bool) {
			var err error
			if lr.HasStep(idx.LoginStepPhoneInitialVerification) {
				lr, err = lr.VerifyPhoneInitial(r.Context(), idx.PhoneMethodSMS, r.FormValue("phoneNumber"))
			} else {
				lr, err = lr.VerifyPhone(r.Context(), idx.PhoneMethodSMS)
			}
			if err != nil {
				session.Values["Errors"] = err.Error()
				session.Save(r, w)
				http.Redirect(w, r, "/login/factors/phone/method", http.StatusFound)
				return
			}
			s.cache.Set("loginResponse", lr, time.Minute*5)
		}
		s.render("loginFactorPhone.gohtml", w, r)
		return
	}
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginPhoneConfirmation(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepPhoneConfirmation) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	lr, err = lr.ConfirmPhone(r.Context(), r.FormValue("code"))
	if err != nil {
		s.ViewData["InvalidPhoneCode"] = true
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/phone", http.StatusFound)
		return
	}
	s.ViewData["InvalidPhoneCode"] = false
	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginOktaVerify(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepOktaVerify) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}

	s.ViewData["OktaVerifyTotp"] = false
	s.ViewData["OktaVerifyPush"] = false
	methodTypes, err := lr.OktaVerifyMethodTypes(r.Context())
	if err == nil {
		for _, mt := range methodTypes {
			switch mt {
			case "push":
				s.ViewData["OktaVerifyPush"] = true
			case "totp":
				s.ViewData["OktaVerifyTotp"] = true
			}
		}
	}

	s.render("loginOktaVerify.gohtml", w, r)
}

func (s *Server) handleLoginOktaVerifyTotp(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepOktaVerify) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	s.render("loginOktaVerifyTotp.gohtml", w, r)
}

func (s *Server) handleLoginOktaVerifyTotpConfirmation(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepOktaVerify) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	lr, err = lr.OktaVerifyConfirm(r.Context(), r.FormValue("code"))
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/okta-verify", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginOktaVerifyPush(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepOktaVerify) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}

	// will block while push login notice sent to remote Okta Verify app
	lr, err := lr.OktaVerify(r.Context())
	if err != nil {
		log.Fatalf("error initiating okta verify async: %s", err)
	}

	s.cache.Set("loginResponse", lr, time.Minute*5)

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginGoogleAuth(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepGoogleAuthenticatorInitialVerification) && !lr.HasStep(idx.LoginStepGoogleAuthenticatorConfirmation) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	if lr.HasStep(idx.LoginStepGoogleAuthenticatorConfirmation) {
		s.render("loginGoogleAuthCode.gohtml", w, r)
		return
	}
	if lr.HasStep(idx.LoginStepGoogleAuthenticatorInitialVerification) {
		http.Redirect(w, r, "/login/factors/google_auth/init", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginGoogleAuthConfirmation(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepGoogleAuthenticatorConfirmation) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	lr, err = lr.GoogleAuthConfirm(r.Context(), r.FormValue("code"))
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/google_auth", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session, err := sessionStore.Get(r, "direct-auth")
		if err != nil {
			log.Fatalf("could not get store: %s", err)
		}
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		// redirect the user to /profile
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginGoogleAuthInit(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepGoogleAuthenticatorInitialVerification) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	lr, err := lr.GoogleAuthInitialVerify(r.Context())
	if err != nil {
		http.Redirect(w, r, "/enrollFactor", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	s.ViewData["QRCode"] = template.URL(lr.ContextualData().QRcode.Href)
	s.ViewData["SharedSecret"] = template.URL(lr.ContextualData().SharedSecret)
	s.render("loginGoogleAuthInitial.gohtml", w, r)
}

func (s *Server) handleLoginWebAuthNChallenge(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepWebAuthNSetup) && !lr.HasStep(idx.LoginStepWebAuthNChallenge) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	if lr.HasStep(idx.LoginStepWebAuthNChallenge) {
		lr, err := lr.WebAuthNChallenge(r.Context())
		if err != nil {
			http.Redirect(w, r, "/login/factors", http.StatusFound)
			return
		}
		s.cache.Set("loginResponse", lr, time.Minute*5)

		s.ViewData["Challenge"] = template.URL(lr.ContextualData().ChallengeData.Challenge)
		s.ViewData["WebauthnCredentialID"] = template.URL(lr.ContextualData().ChallengeData.CredentialID)
		s.render("loginWebAuthN.gohtml", w, r)
		return
	}
	if lr.HasStep(idx.LoginStepWebAuthNSetup) {
		panic("not implemented yet")
		return
	}
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginWebAuthNVerify(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	if clr == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr := clr.(*idx.LoginResponse)
	if !lr.HasStep(idx.LoginStepWebAuthNVerify) {
		http.Redirect(w, r, "/login/factors", http.StatusFound)
		return
	}
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}
	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("could not read request body: %v", err)
	}
	defer r.Body.Close()
	var credentials idx.WebAuthNChallengeCredentials
	if err := json.Unmarshal(reqBody, &credentials); err != nil {
		log.Fatalf("could not unmarshal request body: %v", err)
	}
	lr, err = lr.WebAuthNVerify(r.Context(), &credentials)
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login/factors/web_authn", http.StatusFound)
		return
	}
	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	lr, err = lr.WhereAmI(r.Context())
	if err != nil {
		session.Values["Errors"] = err.Error()
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	s.cache.Set("loginResponse", lr, time.Minute*5)
	http.Redirect(w, r, "/login/factors", http.StatusFound)
}

func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	clr, _ := s.cache.Get("loginResponse")
	s.cache.Delete("loginResponse")
	lr := clr.(*idx.LoginResponse)

	// Get session store so we can store our tokens
	session, err := sessionStore.Get(r, "direct-auth")
	if err != nil {
		log.Fatalf("could not get store: %s", err)
	}

	if code, found := r.URL.Query()["otp"]; found {

		// If the login callback is called with otp and state values we need to
		// check if the idx state string in the session is the same as the login
		// response's context. If not, just display the otp value in a page and
		// ask the user to enter the code in the original browser where they
		// started the login flow login session.
		state, found := session.Values["idxContext.state"]
		if !found || lr.Context().State != state {
			// need to keep the login response resident
			s.cache.Set("loginResponse", lr, time.Minute*5)
			s.ViewData["OTP"] = code[0]
			s.render("loginFactorEmailOtp.gohtml", w, r)
			return
		}

		lr, err = lr.ConfirmEmail(r.Context(), code[0])
		if err != nil {
			log.Fatalf("could not confirm email with otp code %q: %s", code[0], err)
		}
	} else {
		lr, err = lr.WhereAmI(r.Context())
		if err != nil {
			log.Fatalf("could not tell where I am: %s", err)
		}
	}

	// Deal with there aren't any login steps, perhaps user didn't complete enrollment.
	if len(lr.AvailableSteps()) == 0 ||
		(len(lr.AvailableSteps()) == 1 && lr.HasStep(idx.LoginStepCancel)) {
		session, err := sessionStore.Get(r, "direct-auth")
		if err == nil {
			session.Values["Errors"] = "There should be should be additional login factors available but they are not."
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// If we have tokens we have success, so lets store tokens
	if lr.Token() != nil {
		session.Values["access_token"] = lr.Token().AccessToken
		session.Values["id_token"] = lr.Token().IDToken
		err = session.Save(r, w)
		if err != nil {
			log.Fatalf("could not save access token: %s", err)
		}
	} else {
		session.Values["Errors"] = "We expected tokens to be available here but were not. Authentication Failed."
		session.Save(r, w)
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// redirect the user to /profile
	http.Redirect(w, r, "/", http.StatusFound)
}
