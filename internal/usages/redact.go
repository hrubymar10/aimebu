package usages

import "strings"

const redacted = "[redacted]"

func RedactString(s string, secrets ...string) string {
	out := s
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, redacted)
	}
	return out
}

func (s Snapshot) Redacted(secrets ...string) Snapshot {
	s.Error = RedactString(s.Error, secrets...)
	s.Plan = RedactString(s.Plan, secrets...)
	if s.Credits != nil {
		credits := *s.Credits
		credits.Label = RedactString(credits.Label, secrets...)
		s.Credits = &credits
	}
	if s.ErrorDetail != nil {
		fields := make(map[string]string, len(s.ErrorDetail.Fields))
		for key, value := range s.ErrorDetail.Fields {
			fields[RedactString(key, secrets...)] = RedactString(value, secrets...)
		}
		s.ErrorDetail = &ErrorDetail{Fields: fields}
	}
	return s
}

func (p Profile) Redacted(secrets ...string) Profile {
	p.Error = RedactString(p.Error, secrets...)
	p.Plan = RedactString(p.Plan, secrets...)
	if p.Credits != nil {
		credits := *p.Credits
		credits.Label = RedactString(credits.Label, secrets...)
		p.Credits = &credits
	}
	if p.ErrorDetail != nil {
		fields := make(map[string]string, len(p.ErrorDetail.Fields))
		for key, value := range p.ErrorDetail.Fields {
			fields[RedactString(key, secrets...)] = RedactString(value, secrets...)
		}
		p.ErrorDetail = &ErrorDetail{Fields: fields}
	}
	return p
}
