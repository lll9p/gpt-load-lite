package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/outboundproxy"
	"gpt-load/internal/response"
)

func (s *Server) GetOutboundProxySubscription(c *gin.Context) {
	response.Success(c, s.ProxyManager.Subscription())
}

func bindSubscription(c *gin.Context) (outboundproxy.SubscriptionInput, bool) {
	var req outboundproxy.SubscriptionInput
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, app_errors.ErrInvalidJSON)
		return req, false
	}
	return req, true
}

func (s *Server) SaveOutboundProxySubscription(c *gin.Context) {
	req, ok := bindSubscription(c)
	if !ok {
		return
	}
	result, err := s.ProxyManager.SaveSubscription(req)
	if !handleOutboundProxyError(c, err) {
		response.Success(c, result)
	}
}

func (s *Server) PreviewOutboundProxySubscription(c *gin.Context) {
	req, ok := bindSubscription(c)
	if !ok {
		return
	}
	result, err := s.ProxyManager.PreviewSubscription(c.Request.Context(), req)
	if !handleOutboundProxyError(c, err) {
		response.Success(c, result)
	}
}

func (s *Server) RefreshOutboundProxySubscription(c *gin.Context) {
	result, err := s.ProxyManager.RefreshSubscription(c.Request.Context())
	if !handleOutboundProxyError(c, err) {
		response.Success(c, result)
	}
}
