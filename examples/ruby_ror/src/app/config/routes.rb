Rails.application.routes.draw do
  get "up" => "rails/health#show"
  get "/" => "visits#create"
end
