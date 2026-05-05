#!/bin/bash
# setup-firebase.sh - Setup Identity Platform and initialize frontend config

PROJECT_ID=$(gcloud config get-value project)

echo "🔥 Enabling Identity Platform for $PROJECT_ID..."
gcloud services enable identitytoolkit.googleapis.com

mkdir -p /home/justlornt95/frontend/lib

echo "📦 Initializing Firebase client config..."
cat <<EOF > /home/justlornt95/frontend/lib/firebase.js
import { initializeApp } from "firebase/app";
import { getAuth } from "firebase/auth";

const firebaseConfig = {
  apiKey: process.env.NEXT_PUBLIC_FIREBASE_API_KEY,
  authDomain: "${PROJECT_ID}.firebaseapp.com",
  projectId: "${PROJECT_ID}",
  storageBucket: "${PROJECT_ID}.appspot.com",
  appId: process.env.NEXT_PUBLIC_FIREBASE_APP_ID
};

const app = initializeApp(firebaseConfig);
export const auth = getAuth(app);
EOF

echo "✅ Firebase configuration generated at /home/justlornt95/frontend/lib/firebase.js"
echo "👉 Note: Ensure you set NEXT_PUBLIC_FIREBASE_API_KEY and NEXT_PUBLIC_FIREBASE_APP_ID in your .env file."