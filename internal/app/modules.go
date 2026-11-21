package app

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	moduleExample "starter-kit/internal/example"
	// scaffold:imports
)

// registerModules is the composition root. Keep dependency creation here;
// module services only receive the dependencies they need.
func registerModules(api *gin.RouterGroup, db *gorm.DB) {
	{
		var repo moduleExample.Repo = moduleExample.NewMemoryRepo()
		if db != nil {
			repo = moduleExample.NewSQLRepo(db)
		}
		moduleExample.RegisterRoutes(api.Group("/example"), moduleExample.NewService(repo))
	}
	// scaffold:modules
}
