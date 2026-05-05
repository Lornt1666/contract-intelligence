terraform {
  backend "gcs" {
    bucket = "quantum-petal-495301-b0-tfstate"
    prefix = "terraform/state"
  }
}

resource "google_cloud_run_v2_service" "control_plane" {
  name     = "control-plane"
  location = "us-central1"
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    containers {
      image = "us-central1-docker.pkg.dev/quantum-petal-495301-b0/saas-factory-repo/control-plane:latest"
      
      ports {
        container_port = 8080
      }

      # Readiness check: Ensures the app is ready before serving traffic
      startup_probe {
        initial_delay_seconds = 5
        timeout_seconds       = 1
        period_seconds        = 10
        failure_threshold     = 3
        http_get {
          path = "/health"
        }
      }

      # Health check: Monitors the app during its lifecycle
      liveness_probe {
        http_get {
          path = "/health"
        }
      }

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = "quantum-petal-495301-b0"
      }
    }
  }
}