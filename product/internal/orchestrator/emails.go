package orchestrator

// Customer-facing emails are localized to the language the customer used when
// they created the project (project.Locale). Only a couple of templates go to
// customers, so a small local map is simpler than importing the web catalog and
// keeps the copy next to where it's sent. Operator emails stay English.

type emailCopy struct{ Subject, Body string } // Body ends with "\n\n" + link at call site

var customerEmails = map[string]map[string]emailCopy{
	"plan_ready": {
		"en": {"Your plan is ready to approve",
			"Your plan for \"%s\" is ready. Have a look at what's included and approve it to start the build:"},
		"sv": {"Din plan är redo att godkännas",
			"Din plan för \"%s\" är klar. Titta på vad som ingår och godkänn den så sätter vi igång bygget:"},
		"ru": {"Ваш план готов к утверждению",
			"Ваш план для «%s» готов. Посмотрите, что входит, и утвердите его, чтобы начать сборку:"},
	},
	"preview_ready": {
		"en": {"Your website preview is ready",
			"Your preview for \"%s\" is ready to view. Open your project to review it or request a change:"},
		"sv": {"Din förhandsvisning är klar",
			"Din förhandsvisning för \"%s\" är klar. Öppna ditt projekt för att granska den eller begära en ändring:"},
		"ru": {"Превью вашего сайта готово",
			"Превью для «%s» готово. Откройте проект, чтобы просмотреть его или запросить изменение:"},
	},
	"subscription_active": {
		"en": {"Thanks for subscribing — your site is on its way",
			"Your subscription for \"%s\" is active. Rasmus is giving it a final personal review and will deliver it shortly:"},
		"sv": {"Tack för din prenumeration — din sida är på väg",
			"Din prenumeration för \"%s\" är aktiv. Rasmus gör en sista personlig granskning och levererar den inom kort:"},
		"ru": {"Спасибо за подписку — ваш сайт уже в пути",
			"Ваша подписка для «%s» активна. Rasmus проводит финальную личную проверку и вскоре доставит сайт:"},
	},
}

// custEmail returns localized subject + body for a customer email, defaulting
// to English for an unknown locale or key.
func custEmail(locale, key string) emailCopy {
	byLang := customerEmails[key]
	if c, ok := byLang[locale]; ok {
		return c
	}
	return byLang["en"]
}
